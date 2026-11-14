package service

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"

	"github.com/tidwall/gjson"
)

func ExtractContentModerationText(protocol string, body []byte) string {
	return ExtractContentModerationInput(protocol, body).Text
}

func ExtractContentModerationInput(protocol string, body []byte) ContentModerationInput {
	return extractContentModerationInput(protocol, body, true, false)
}

// Keyword checks share semantic moderation's current-user boundaries, but must
// inspect client-supplied reminder blocks, OpenAI Responses instructions, and
// trailing system/developer messages as ordinary text.
func extractContentModerationKeywordText(protocol string, body []byte) string {
	return extractContentModerationInput(protocol, body, false, true).Text
}

type moderationTextCollector struct {
	filterReminders        bool
	includeKeywordBypasses bool
}

func extractContentModerationInput(protocol string, body []byte, filterReminders bool, includeKeywordBypasses bool) ContentModerationInput {
	collector := moderationTextCollector{
		filterReminders:        filterReminders,
		includeKeywordBypasses: includeKeywordBypasses,
	}
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ContentModerationInput{}
	}
	var parts []string
	var images []string
	switch protocol {
	case ContentModerationProtocolAnthropicMessages:
		collector.collectAnthropicPayload(body, &parts, &images)
	case ContentModerationProtocolOpenAIChat:
		collector.collectLastRoleMessage(gjson.GetBytes(body, "messages"), "user", &parts, &images)
	case ContentModerationProtocolOpenAIResponses:
		collector.collectResponsesPayload(body, &parts, &images)
	case ContentModerationProtocolGemini:
		collector.collectGeminiPayload(body, &parts, &images)
	case ContentModerationProtocolOpenAIImages:
		collector.addModerationText(&parts, gjson.GetBytes(body, "prompt").String())
		collector.collectContentValue(gjson.GetBytes(body, "images"), &parts, &images)
	case ContentModerationProtocolTypeSafeSystemOne:
		// System One carries no client-harness reminder blocks, so a literal
		// <system-reminder> is ordinary user text and must never be skipped.
		moderationTextCollector{}.collectSystemOneInput(body, &parts)
	default:
		collector.collectResponsesPayload(body, &parts, &images)
		collector.collectLastRoleMessage(gjson.GetBytes(body, "messages"), "user", &parts, &images)
		collector.collectGeminiPayload(body, &parts, &images)
	}
	out := ContentModerationInput{
		Text:   normalizeContentModerationText(strings.Join(parts, "\n")),
		Images: normalizeModerationImages(images),
	}
	if filterReminders {
		out.Normalize()
	}
	return out
}

// collectSystemOneInput moderates every client-controlled text of a System One
// request: question IDs, every question field except the validated type,
// unknown top-level extension fields, and the evaluated state. Object keys are
// sent to Jev as part of the JSON, so they are moderated like values.
func (collector moderationTextCollector) collectSystemOneInput(body []byte, parts *[]string) {
	root := gjson.ParseBytes(body)
	questions := root.Get("questions")
	if !questions.IsObject() {
		collector.collectSystemOneText(questions, parts)
	}
	questions.ForEach(func(id, question gjson.Result) bool {
		collector.addModerationText(parts, id.String())
		if !question.IsObject() {
			collector.collectSystemOneText(question, parts)
			return true
		}
		question.ForEach(func(field, value gjson.Result) bool {
			switch field.String() {
			case "type":
				return true
			case "instructions", "criteria":
			default:
				collector.addModerationText(parts, field.String())
			}
			collector.collectSystemOneText(value, parts)
			return true
		})
		return true
	})
	root.ForEach(func(field, value gjson.Result) bool {
		switch field.String() {
		case "model", "stream", "state", "questions":
			return true
		}
		collector.addModerationText(parts, field.String())
		collector.collectSystemOneText(value, parts)
		return true
	})
	collector.collectSystemOneText(root.Get("state"), parts)
}

func (collector moderationTextCollector) collectSystemOneText(value gjson.Result, parts *[]string) {
	switch {
	case !value.Exists():
		return
	case value.Type == gjson.String:
		collector.addModerationText(parts, value.String())
	case value.IsArray():
		value.ForEach(func(_, child gjson.Result) bool {
			collector.collectSystemOneText(child, parts)
			return true
		})
	case value.IsObject():
		value.ForEach(func(key, child gjson.Result) bool {
			collector.addModerationText(parts, key.String())
			collector.collectSystemOneText(child, parts)
			return true
		})
	}
}

func isTrailingInstructionRole(item gjson.Result) bool {
	switch strings.ToLower(strings.TrimSpace(item.Get("role").String())) {
	case "system", "developer":
		return true
	default:
		return false
	}
}

func (collector moderationTextCollector) collectLastRoleMessage(messages gjson.Result, role string, parts *[]string, images *[]string) {
	if !messages.IsArray() {
		return
	}
	array := messages.Array()
	if len(array) == 0 {
		return
	}
	lastIdx := len(array) - 1
	if collector.includeKeywordBypasses {
		for lastIdx >= 0 && isTrailingInstructionRole(array[lastIdx]) {
			lastIdx--
		}
		// Only trailing system/developer items when no preceding non-user turn ended the loop.
		if lastIdx < 0 {
			for _, item := range array {
				collector.collectContentValue(item.Get("content"), parts, images)
			}
			return
		}
	}
	last := array[lastIdx]
	if strings.ToLower(strings.TrimSpace(last.Get("role").String())) != role {
		return
	}
	var candidate []string
	var candidateImages []string
	collector.collectContentValue(last.Get("content"), &candidate, &candidateImages)
	if collector.includeKeywordBypasses {
		for j := lastIdx + 1; j < len(array); j++ {
			collector.collectContentValue(array[j].Get("content"), &candidate, &candidateImages)
		}
	}
	if normalizeContentModerationText(strings.Join(candidate, "\n")) == "" && len(candidateImages) == 0 {
		return
	}
	*parts = append(*parts, candidate...)
	*images = append(*images, candidateImages...)
}

func (collector moderationTextCollector) collectAnthropicPayload(body []byte, parts *[]string, images *[]string) {
	agentLoopEnded := collector.collectLastAnthropicUserMessage(gjson.GetBytes(body, "messages"), parts, images)
	if collector.includeKeywordBypasses && !agentLoopEnded {
		if sys := gjson.GetBytes(body, "system"); sys.Exists() {
			collector.collectAnthropicUserContentValue(sys, parts, images)
		}
	}
}

func hasAnthropicToolResult(content gjson.Result) bool {
	if content.IsObject() {
		return strings.EqualFold(strings.TrimSpace(content.Get("type").String()), "tool_result")
	}
	if !content.IsArray() {
		return false
	}
	found := false
	content.ForEach(func(_, item gjson.Result) bool {
		if strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "tool_result") {
			found = true
			return false
		}
		return true
	})
	return found
}

func (collector moderationTextCollector) collectLastAnthropicUserMessage(messages gjson.Result, parts *[]string, images *[]string) bool {
	if !messages.IsArray() {
		return false
	}
	array := messages.Array()
	if len(array) == 0 {
		return false
	}
	// Anthropic requests may append a system message after the user turn. It
	// does not represent a new model/tool turn, so retain the latest user
	// content while continuing to reject assistant/tool-ended loops.
	lastUser := len(array) - 1
	for lastUser >= 0 && strings.ToLower(strings.TrimSpace(array[lastUser].Get("role").String())) == "system" {
		lastUser--
	}
	if lastUser < 0 {
		if collector.includeKeywordBypasses {
			for _, item := range array {
				collector.collectAnthropicUserContentValue(item.Get("content"), parts, images)
			}
		}
		return false
	}
	if strings.ToLower(strings.TrimSpace(array[lastUser].Get("role").String())) != "user" {
		return true
	}
	var candidate []string
	var candidateImages []string
	userContent := array[lastUser].Get("content")
	collector.collectAnthropicUserContentValue(userContent, &candidate, &candidateImages)
	if hasAnthropicToolResult(userContent) && normalizeContentModerationText(strings.Join(candidate, "\n")) == "" && len(candidateImages) == 0 {
		return true
	}
	if collector.includeKeywordBypasses {
		for j := lastUser + 1; j < len(array); j++ {
			collector.collectAnthropicUserContentValue(array[j].Get("content"), &candidate, &candidateImages)
		}
	}
	if normalizeContentModerationText(strings.Join(candidate, "\n")) == "" && len(candidateImages) == 0 {
		return false
	}
	*parts = append(*parts, candidate...)
	*images = append(*images, candidateImages...)
	return false
}

func (collector moderationTextCollector) collectAnthropicUserContentValue(value gjson.Result, parts *[]string, images *[]string) {
	switch {
	case !value.Exists():
		return
	case value.Type == gjson.String:
		if !collector.filterReminders || !isAnthropicSystemReminderText(value.String()) {
			collector.addModerationText(parts, value.String())
		}
	case value.IsArray():
		value.ForEach(func(_, item gjson.Result) bool {
			collector.collectAnthropicUserContentValue(item, parts, images)
			return true
		})
	case value.IsObject():
		typ := strings.ToLower(strings.TrimSpace(value.Get("type").String()))
		switch typ {
		case "", "text", "input_text", "message":
			if value.Get("text").Exists() && (!collector.filterReminders || !isAnthropicSystemReminderText(value.Get("text").String())) {
				collector.addModerationText(parts, value.Get("text").String())
			}
			if value.Get("content").Exists() {
				collector.collectAnthropicUserContentValue(value.Get("content"), parts, images)
			}
		case "image_url", "input_image", "image":
			collector.collectContentValue(value, parts, images)
		}
	}
}

func isAnthropicSystemReminderText(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "<system-reminder>")
}

func (collector moderationTextCollector) collectResponsesPayload(body []byte, parts *[]string, images *[]string) {
	input := gjson.GetBytes(body, "input")
	agentLoopEnded := collector.collectLastResponsesInput(input, parts, images)
	if collector.includeKeywordBypasses && !agentLoopEnded {
		if instructions := gjson.GetBytes(body, "instructions"); instructions.Exists() {
			collector.collectContentValue(instructions, parts, images)
		}
	}
}

func (collector moderationTextCollector) collectLastResponsesInput(input gjson.Result, parts *[]string, images *[]string) bool {
	switch {
	case !input.Exists():
		return false
	case input.Type == gjson.String:
		collector.addModerationText(parts, input.String())
		return false
	case input.IsArray():
		array := input.Array()
		if len(array) == 0 {
			return false
		}
		lastIdx := len(array) - 1
		if collector.includeKeywordBypasses {
			for lastIdx >= 0 && isTrailingInstructionRole(array[lastIdx]) {
				lastIdx--
			}
			if lastIdx < 0 {
				for _, item := range array {
					collector.collectResponsesItemText(item, parts, images)
				}
				return false
			}
		}
		last := array[lastIdx]
		if !collector.isResponsesUserTurnItem(last) {
			return true
		}
		collector.collectResponsesItemText(last, parts, images)
		if collector.includeKeywordBypasses {
			for j := lastIdx + 1; j < len(array); j++ {
				collector.collectResponsesItemText(array[j], parts, images)
			}
		}
		return false
	case input.IsObject():
		if collector.isResponsesUserTextItem(input) {
			collector.collectResponsesItemText(input, parts, images)
			return false
		}
		return true
	default:
		return false
	}
}

func (collector moderationTextCollector) collectResponsesItemText(item gjson.Result, parts *[]string, images *[]string) {
	collector.collectContentValue(item.Get("content"), parts, images)
	if item.Get("type").String() == "input_text" || item.Get("text").Exists() {
		collector.collectContentValue(item, parts, images)
	}
}

func (collector moderationTextCollector) isResponsesUserTurnItem(item gjson.Result) bool {
	if collector.includeKeywordBypasses {
		role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
		typ := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
		if role == "user" && (typ == "" || typ == "message" || typ == "input_text") && !hasAnthropicToolResult(item.Get("content")) {
			return true
		}
	}
	return collector.isResponsesUserTextItem(item)
}

func (collector moderationTextCollector) isResponsesUserTextItem(item gjson.Result) bool {
	role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
	if role == "user" {
		return collector.responseItemHasModerationText(item)
	}
	if role != "" {
		return false
	}
	return collector.responseItemHasModerationText(item)
}

func (collector moderationTextCollector) responseItemHasModerationText(item gjson.Result) bool {
	var parts []string
	var images []string
	collector.collectContentValue(item.Get("content"), &parts, &images)
	if item.Get("type").String() == "input_text" || item.Get("text").Exists() {
		collector.collectContentValue(item, &parts, &images)
	}
	return normalizeContentModerationText(strings.Join(parts, "\n")) != "" || len(images) > 0
}

func (collector moderationTextCollector) collectGeminiPayload(body []byte, parts *[]string, images *[]string) {
	agentLoopEnded := collector.collectLastGeminiContent(gjson.GetBytes(body, "contents"), parts, images)
	if collector.includeKeywordBypasses && !agentLoopEnded {
		for _, key := range []string{"systemInstruction", "system_instruction"} {
			if sys := gjson.GetBytes(body, key); sys.Exists() {
				collector.collectGeminiContentParts(sys, parts, images)
			}
		}
	}
}

func hasGeminiFunctionResponse(parts gjson.Result) bool {
	if !parts.IsArray() {
		return false
	}
	found := false
	parts.ForEach(func(_, part gjson.Result) bool {
		if part.Get("functionResponse").Exists() || part.Get("function_response").Exists() {
			found = true
			return false
		}
		return true
	})
	return found
}

func (collector moderationTextCollector) collectGeminiContentParts(node gjson.Result, parts *[]string, images *[]string) {
	if !node.Exists() {
		return
	}
	if node.Type == gjson.String {
		collector.addModerationText(parts, node.String())
		return
	}
	if arr := node.Get("parts"); arr.IsArray() {
		arr.ForEach(func(_, part gjson.Result) bool {
			collector.addModerationText(parts, part.Get("text").String())
			addGeminiModerationImage(images, part)
			return true
		})
	}
}

func (collector moderationTextCollector) collectLastGeminiContent(contents gjson.Result, parts *[]string, images *[]string) bool {
	if !contents.IsArray() {
		return false
	}
	array := contents.Array()
	if len(array) == 0 {
		return false
	}
	last := array[len(array)-1]
	role := strings.ToLower(strings.TrimSpace(last.Get("role").String()))
	if role != "" && role != "user" {
		return true
	}
	var candidate []string
	var candidateImages []string
	partsNode := last.Get("parts")
	collector.collectGeminiContentParts(last, &candidate, &candidateImages)
	if normalizeContentModerationText(strings.Join(candidate, "\n")) == "" && len(candidateImages) == 0 {
		return hasGeminiFunctionResponse(partsNode)
	}
	*parts = append(*parts, candidate...)
	*images = append(*images, candidateImages...)
	return false
}

func (collector moderationTextCollector) collectContentValue(value gjson.Result, parts *[]string, images *[]string) {
	switch {
	case !value.Exists():
		return
	case value.Type == gjson.String:
		collector.addModerationText(parts, value.String())
	case value.IsArray():
		value.ForEach(func(_, item gjson.Result) bool {
			collector.collectContentValue(item, parts, images)
			return true
		})
	case value.IsObject():
		typ := strings.ToLower(strings.TrimSpace(value.Get("type").String()))
		addModerationImage(images, value.Get("image_url.url").String())
		addModerationImage(images, value.Get("image_url").String())
		addModerationImage(images, value.Get("url").String())
		addModerationImageData(images, value.Get("source.media_type").String(), value.Get("source.data").String())
		addModerationImageData(images, value.Get("source.mediaType").String(), value.Get("source.data").String())
		addModerationImageData(images, value.Get("media_type").String(), value.Get("data").String())
		addModerationImageData(images, value.Get("mime_type").String(), value.Get("data").String())
		addModerationImageData(images, value.Get("mimeType").String(), value.Get("data").String())
		addModerationImage(images, value.Get("source.data").String())
		addModerationImage(images, value.Get("data").String())
		addModerationImage(images, value.Get("base64").String())
		switch typ {
		case "", "text", "input_text", "message":
			if value.Get("text").Exists() {
				collector.addModerationText(parts, value.Get("text").String())
			}
			if value.Get("content").Exists() {
				collector.collectContentValue(value.Get("content"), parts, images)
			}
		case "image_url", "input_image", "image":
		}
	}
}

func addGeminiModerationImage(images *[]string, part gjson.Result) {
	if inlineData := part.Get("inline_data"); inlineData.IsObject() {
		mimeType := strings.TrimSpace(inlineData.Get("mime_type").String())
		data := strings.TrimSpace(inlineData.Get("data").String())
		if mimeType != "" && data != "" {
			addModerationImage(images, fmt.Sprintf("data:%s;base64,%s", mimeType, data))
		}
	}
	if inlineData := part.Get("inlineData"); inlineData.IsObject() {
		mimeType := strings.TrimSpace(inlineData.Get("mimeType").String())
		data := strings.TrimSpace(inlineData.Get("data").String())
		if mimeType != "" && data != "" {
			addModerationImage(images, fmt.Sprintf("data:%s;base64,%s", mimeType, data))
		}
	}
	addModerationImage(images, part.Get("file_data.file_uri").String())
	addModerationImage(images, part.Get("fileData.fileUri").String())
}

func addModerationImageData(images *[]string, mimeType string, data string) {
	mimeType = strings.TrimSpace(mimeType)
	data = strings.TrimSpace(data)
	if mimeType == "" || data == "" {
		return
	}
	addModerationImage(images, fmt.Sprintf("data:%s;base64,%s", mimeType, data))
}

func addModerationImage(images *[]string, image string) {
	image = strings.TrimSpace(image)
	if image == "" {
		return
	}
	if strings.HasPrefix(image, "data:") || strings.HasPrefix(image, "http://") || strings.HasPrefix(image, "https://") {
		*images = append(*images, image)
	}
}

func normalizeModerationImages(images []string) []string {
	out := make([]string, 0, len(images))
	seen := make(map[string]struct{}, len(images))
	for _, image := range images {
		image = strings.TrimSpace(image)
		if image == "" {
			continue
		}
		if _, ok := seen[image]; ok {
			continue
		}
		seen[image] = struct{}{}
		out = append(out, image)
	}
	return out
}

func limitContentModerationImages(images []string) []string {
	if len(images) <= maxContentModerationInputImages {
		return images
	}
	idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(images))))
	if err != nil {
		return images[:maxContentModerationInputImages]
	}
	return []string{images[int(idx.Int64())]}
}

func (collector moderationTextCollector) addModerationText(parts *[]string, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if collector.filterReminders && strings.Contains(text, "<system-reminder>") {
		return
	}
	*parts = append(*parts, text)
}

func normalizeContentModerationText(text string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
}
