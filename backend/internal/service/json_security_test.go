package service

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateJSONNoDuplicateKeys(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantError bool
	}{
		{name: "unique nested objects", body: `{"input":"safe","nested":{"a":1,"b":[{"c":true}]}}`},
		{name: "top level duplicate", body: `{"input":"safe","input":"unsafe"}`, wantError: true},
		{name: "nested duplicate", body: `{"commands":{"search_query":[{"q":"safe","q":"unsafe"}]}}`, wantError: true},
		{name: "escaped duplicate spelling", body: `{"in\u0070ut":"safe","input":"unsafe"}`, wantError: true},
		{name: "same key in separate objects is valid", body: `{"left":{"value":1},"right":{"value":2}}`},
		{name: "duplicate identical string value is allowed", body: `{"model":"gpt-5.4","model":"gpt-5.4"}`},
		{name: "duplicate identical nested object is allowed", body: `{"meta":{"a":1},"meta":{"a":1}}`},
		{name: "duplicate conflicting nested object is rejected", body: `{"meta":{"a":1},"meta":{"a":2}}`, wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateJSONNoDuplicateKeys([]byte(tt.body))
			if tt.wantError {
				require.Error(t, err)
				require.True(t, errors.Is(err, ErrDuplicateJSONKeys))
				require.Equal(t, DuplicateJSONKeysMessage, err.Error())
				return
			}
			require.NoError(t, err)
		})
	}
}
