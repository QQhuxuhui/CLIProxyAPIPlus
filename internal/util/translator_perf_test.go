package util

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type renameKeyCase struct {
	name       string
	payload    string
	oldKeyPath string
	newKeyPath string
}

// renameKeyCorpus covers the shapes RenameKey is used with across the
// translators: tool parameter schemas, response schemas and nested paths.
func renameKeyCorpus() []renameKeyCase {
	return []renameKeyCase{
		{
			name:       "leaf_object_value",
			payload:    `{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}},"description":"d"}`,
			oldKeyPath: "parameters",
			newKeyPath: "parametersJsonSchema",
		},
		{
			name:       "first_key",
			payload:    `{"parameters":{"type":"object"},"name":"a"}`,
			oldKeyPath: "parameters",
			newKeyPath: "parametersJsonSchema",
		},
		{
			name:       "only_key",
			payload:    `{"parameters":{"type":"object"}}`,
			oldKeyPath: "parameters",
			newKeyPath: "parametersJsonSchema",
		},
		{
			name:       "nested_path",
			payload:    `{"request":{"tools":[{"function_declarations":[{"name":"a","parameters":{"type":"object"}},{"name":"b","parameters":{"type":"string"}}]}]}}`,
			oldKeyPath: "request.tools.0.function_declarations.1.parameters",
			newKeyPath: "request.tools.0.function_declarations.1.parametersJsonSchema",
		},
		{
			name:       "generation_config_response_schema",
			payload:    `{"generationConfig":{"temperature":0.5,"responseSchema":{"type":"object","properties":{"a":{"type":"number"}}}}}`,
			oldKeyPath: "generationConfig.responseSchema",
			newKeyPath: "generationConfig.responseJsonSchema",
		},
		{
			name:       "camel_to_snake_array_value",
			payload:    `{"tools":[{"functionDeclarations":[{"name":"a"}]}]}`,
			oldKeyPath: "tools.0.functionDeclarations",
			newKeyPath: "tools.0.function_declarations",
		},
		{
			name:       "string_value",
			payload:    `{"a":"text with \" quote and 🚀","b":1}`,
			oldKeyPath: "a",
			newKeyPath: "aRenamed",
		},
		{
			name:       "number_value",
			payload:    `{"a":42,"b":1}`,
			oldKeyPath: "a",
			newKeyPath: "aRenamed",
		},
		{
			name:       "null_value",
			payload:    `{"a":null,"b":1}`,
			oldKeyPath: "a",
			newKeyPath: "aRenamed",
		},
		{
			name:       "bool_value",
			payload:    `{"a":true,"b":1}`,
			oldKeyPath: "a",
			newKeyPath: "aRenamed",
		},
		{
			name:       "target_key_already_exists",
			payload:    `{"a":1,"b":2}`,
			oldKeyPath: "a",
			newKeyPath: "b",
		},
		{
			name:       "missing_old_key",
			payload:    `{"a":1}`,
			oldKeyPath: "missing",
			newKeyPath: "renamed",
		},
		{
			name:       "missing_nested_old_key",
			payload:    `{"a":{"b":1}}`,
			oldKeyPath: "a.missing",
			newKeyPath: "a.renamed",
		},
		{
			name: "pretty_printed",
			payload: `{
  "name": "read_file",
  "parameters": {
    "type": "object",
    "properties": {"path": {"type": "string"}}
  },
  "description": "d"
}`,
			oldKeyPath: "parameters",
			newKeyPath: "parametersJsonSchema",
		},
		{
			name:       "large_declaration",
			payload:    buildToolDeclarationsPayload(12, 2048),
			oldKeyPath: "request.tools.0.function_declarations.6.parameters",
			newKeyPath: "request.tools.0.function_declarations.6.parametersJsonSchema",
		},
	}
}

// buildToolDeclarationsPayload renders a request whose tool declarations carry
// large JSON schemas.
func buildToolDeclarationsPayload(declarations, textSize int) string {
	filler := strings.Repeat("lorem ipsum dolor sit amet ", (textSize/27)+1)[:textSize]

	var b strings.Builder
	b.WriteString(`{"project":"","model":"gemini-3-pro-preview","request":{"contents":[{"role":"user","parts":[{"text":"` + filler + `"}]}],"tools":[{"function_declarations":[`)
	for i := 0; i < declarations; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"name":"tool_%d","description":"%s","parameters":{"type":"object","properties":{"path":{"type":"string","description":"%s"},"limit":{"type":"integer"}},"required":["path"]}}`, i, filler, filler)
	}
	b.WriteString(`]}]}}`)
	return b.String()
}

func TestRenameKeyMatchesLegacy(t *testing.T) {
	byteIdentical := 0
	corpus := renameKeyCorpus()
	for _, testCase := range corpus {
		t.Run(testCase.name, func(t *testing.T) {
			legacyOut, legacyErr := legacyRenameKey(testCase.payload, testCase.oldKeyPath, testCase.newKeyPath)
			newOut, newErr := RenameKey(testCase.payload, testCase.oldKeyPath, testCase.newKeyPath)

			if (legacyErr == nil) != (newErr == nil) {
				t.Fatalf("error mismatch: legacy %v, new %v", legacyErr, newErr)
			}
			if legacyErr != nil {
				if legacyErr.Error() != newErr.Error() {
					t.Fatalf("error text mismatch: legacy %q, new %q", legacyErr, newErr)
				}
				if legacyOut != newOut {
					t.Fatalf("error output mismatch: legacy %q, new %q", legacyOut, newOut)
				}
				byteIdentical++
				return
			}

			var legacyValue, newValue any
			if err := json.Unmarshal([]byte(legacyOut), &legacyValue); err != nil {
				t.Fatalf("legacy output is not valid JSON: %v", err)
			}
			if err := json.Unmarshal([]byte(newOut), &newValue); err != nil {
				t.Fatalf("new output is not valid JSON: %v", err)
			}
			if !reflect.DeepEqual(legacyValue, newValue) {
				t.Fatalf("JSON mismatch\nlegacy: %s\nnew:    %s", legacyOut, newOut)
			}
			if legacyOut == newOut {
				byteIdentical++
			} else {
				t.Logf("semantically equal but not byte-identical\nlegacy: %s\nnew:    %s", legacyOut, newOut)
			}
		})
	}
	t.Logf("byte-identical cases: %d/%d", byteIdentical, len(corpus))
}

func TestRenameKeyBytesMatchesRenameKey(t *testing.T) {
	for _, testCase := range renameKeyCorpus() {
		input := []byte(testCase.payload)
		stringOut, stringErr := RenameKey(testCase.payload, testCase.oldKeyPath, testCase.newKeyPath)
		bytesOut, bytesErr := RenameKeyBytes(input, testCase.oldKeyPath, testCase.newKeyPath)

		if (stringErr == nil) != (bytesErr == nil) {
			t.Fatalf("%s: error mismatch: string %v, bytes %v", testCase.name, stringErr, bytesErr)
		}
		if string(input) != testCase.payload {
			t.Fatalf("%s: input buffer was modified in place", testCase.name)
		}
		if bytesErr != nil {
			continue
		}
		if stringOut != string(bytesOut) {
			t.Errorf("%s: RenameKeyBytes output differs\nstring: %s\nbytes:  %s", testCase.name, stringOut, bytesOut)
		}
	}
}

func benchmarkRenamePayload(b *testing.B) []byte {
	b.Helper()
	payload := []byte(buildToolDeclarationsPayload(12, 24000))
	b.Logf("benchmark payload size: %d bytes", len(payload))
	return payload
}

const (
	benchmarkOldKeyPath = "request.tools.0.function_declarations.6.parameters"
	benchmarkNewKeyPath = "request.tools.0.function_declarations.6.parametersJsonSchema"
)

// BenchmarkRenameKeyLegacy measures the call pattern used by the translators:
// bytes in, bytes out, through the string based helper.
func BenchmarkRenameKeyLegacy(b *testing.B) {
	payload := benchmarkRenamePayload(b)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		renamed, _ := legacyRenameKey(string(payload), benchmarkOldKeyPath, benchmarkNewKeyPath)
		_ = []byte(renamed)
	}
}

func BenchmarkRenameKeyNew(b *testing.B) {
	payload := benchmarkRenamePayload(b)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		renamed, _ := RenameKey(string(payload), benchmarkOldKeyPath, benchmarkNewKeyPath)
		_ = []byte(renamed)
	}
}

func BenchmarkRenameKeyBytes(b *testing.B) {
	payload := benchmarkRenamePayload(b)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = RenameKeyBytes(payload, benchmarkOldKeyPath, benchmarkNewKeyPath)
	}
}
