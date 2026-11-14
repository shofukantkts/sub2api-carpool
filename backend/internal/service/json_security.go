package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const DuplicateJSONKeysMessage = "Invalid JSON: duplicate keys are not permitted"

var ErrDuplicateJSONKeys = errors.New("invalid JSON: duplicate keys are not permitted")

// ValidateJSONNoDuplicateKeys validates a JSON document and rejects conflicting
// duplicate object members at every nesting level. Identical repeated keys with
// matching values are permitted (consistent with upstream allowlist handling),
// while conflicting values are rejected to prevent parser discrepancy /
// request-smuggling bypasses between moderation and upstream parsers.
func ValidateJSONNoDuplicateKeys(body []byte) error {
	if !json.Valid(body) {
		return errors.New("invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := walkJSONWithoutDuplicateKeys(decoder); err != nil {
		return err
	}

	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("invalid JSON: multiple top-level values")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func walkJSONWithoutDuplicateKeys(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}

	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return nil
	}

	switch delim {
	case '{':
		seen := make(map[string][]byte)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("invalid JSON: %w", err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("invalid JSON: object key is not a string")
			}
			var rawVal json.RawMessage
			if err := decoder.Decode(&rawVal); err != nil {
				return fmt.Errorf("invalid JSON: %w", err)
			}
			trimmedVal := bytes.TrimSpace(rawVal)
			if prevVal, exists := seen[key]; exists {
				if !bytes.Equal(trimmedVal, prevVal) {
					return ErrDuplicateJSONKeys
				}
			} else {
				seen[key] = trimmedVal
			}
			if len(trimmedVal) > 0 && (trimmedVal[0] == '{' || trimmedVal[0] == '[') {
				childDecoder := json.NewDecoder(bytes.NewReader(trimmedVal))
				if err := walkJSONWithoutDuplicateKeys(childDecoder); err != nil {
					return err
				}
			}
		}
		if _, err := decoder.Token(); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
	case '[':
		for decoder.More() {
			var rawVal json.RawMessage
			if err := decoder.Decode(&rawVal); err != nil {
				return fmt.Errorf("invalid JSON: %w", err)
			}
			trimmedVal := bytes.TrimSpace(rawVal)
			if len(trimmedVal) > 0 && (trimmedVal[0] == '{' || trimmedVal[0] == '[') {
				childDecoder := json.NewDecoder(bytes.NewReader(trimmedVal))
				if err := walkJSONWithoutDuplicateKeys(childDecoder); err != nil {
					return err
				}
			}
		}
		if _, err := decoder.Token(); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
	}
	return nil
}
