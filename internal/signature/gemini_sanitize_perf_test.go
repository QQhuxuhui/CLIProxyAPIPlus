package signature

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

// perfCompatibleSignature builds a Gemini 3 shaped signature that the policy
// preserves as-is.
func perfCompatibleSignature(seed string) string {
	payload := []byte("gemini-thought-" + seed)
	for len(payload) < 96 {
		payload = append(payload, byte(len(payload)))
	}
	var inner []byte
	inner = protowire.AppendTag(inner, 1, protowire.BytesType)
	inner = protowire.AppendBytes(inner, payload)
	var outer []byte
	outer = protowire.AppendTag(outer, 2, protowire.BytesType)
	outer = protowire.AppendBytes(outer, inner)
	return base64.StdEncoding.EncodeToString(outer)
}

// perfBypassSignature builds a base64 UUID signature that the policy replaces
// with the bypass sentinel.
func perfBypassSignature() string {
	return base64.StdEncoding.EncodeToString([]byte("e24830a7-5cd6-42fe-998b-ee539e72b9c3"))
}

type geminiSanitizeCase struct {
	name         string
	contentsPath string
	payload      string
}

// geminiSanitizeCorpus returns realistic Gemini request payloads covering every
// signature location, role shape and container variation the sanitizer sees.
func geminiSanitizeCorpus() []geminiSanitizeCase {
	compatible := perfCompatibleSignature("a")
	bypass := perfBypassSignature()

	cases := []geminiSanitizeCase{
		{name: "no_contents", payload: `{"model":"gemini-3-pro-preview"}`},
		{name: "contents_not_array", payload: `{"contents":{"role":"model"}}`},
		{name: "contents_empty", payload: `{"contents":[]}`},
		{name: "user_text_only", payload: `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`},
		{name: "model_text_without_signature", payload: `{"contents":[{"role":"model","parts":[{"text":"answer"}]}]}`},
		{name: "parts_not_array", payload: `{"contents":[{"role":"model","parts":{"text":"answer"}}]}`},
		{name: "parts_scalar_entries", payload: `{"contents":[{"role":"model","parts":["text",3,null]}]}`},
		{name: "empty_part_object", payload: `{"contents":[{"role":"model","parts":[{}]}]}`},
		{
			name:    "model_function_call_camel_signature",
			payload: `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{"path":"a.go"}},"thoughtSignature":"` + bypass + `"}]}]}`,
		},
		{
			name:    "model_function_call_snake_signature",
			payload: `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{"path":"a.go"}},"thought_signature":"` + bypass + `"}]}]}`,
		},
		{
			name:    "model_function_call_nested_camel_signature",
			payload: `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{},"thoughtSignature":"` + bypass + `"}}]}]}`,
		},
		{
			name:    "model_function_call_nested_snake_signature",
			payload: `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{},"thought_signature":"` + bypass + `"}}]}]}`,
		},
		{
			name:    "model_extra_content_google_signature",
			payload: `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{}},"extra_content":{"google":{"thought_signature":"` + bypass + `"}}}]}]}`,
		},
		{
			name:    "model_all_signature_paths",
			payload: `{"contents":[{"role":"model","parts":[{"thoughtSignature":"` + bypass + `","thought_signature":"x","functionCall":{"name":"f","args":{},"thoughtSignature":"y","thought_signature":"z"},"extra_content":{"google":{"thought_signature":"w"},"other":1}}]}]}`,
		},
		{
			name:    "model_signature_is_first_key",
			payload: `{"contents":[{"role":"model","parts":[{"thoughtSignature":"` + bypass + `","functionCall":{"name":"f","args":{}}}]}]}`,
		},
		{
			name:    "model_signature_is_only_key",
			payload: `{"contents":[{"role":"model","parts":[{"thoughtSignature":"` + bypass + `"}]}]}`,
		},
		{
			name:    "model_compatible_signature_preserved",
			payload: `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"` + compatible + `"}]}]}`,
		},
		{
			name:    "model_thought_part",
			payload: `{"contents":[{"role":"model","parts":[{"text":"reasoning","thought":true,"thoughtSignature":"` + compatible + `"},{"text":"answer"}]}]}`,
		},
		{
			name:    "function_response_with_signatures",
			payload: `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"result":"ok"},"thoughtSignature":"bad"},"thoughtSignature":"bad"}]}]}`,
		},
		{
			name:    "function_response_without_signature",
			payload: `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"result":"ok"}}}]}]}`,
		},
		{
			name:    "function_response_in_model_turn",
			payload: `{"contents":[{"role":"model","parts":[{"functionResponse":{"name":"f","response":{"result":"ok"},"thought_signature":"bad"}}]}]}`,
		},
		{
			name:    "missing_role_with_signature",
			payload: `{"contents":[{"parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"` + bypass + `"}]}]}`,
		},
		{
			name:    "unicode_and_escapes",
			payload: `{"contents":[{"role":"model","parts":[{"text":"quote \" emoji 🚀 backslash \\ tab \t","thoughtSignature":"` + bypass + `"}]}]}`,
		},
		{
			name:         "nested_contents_path",
			contentsPath: "request.contents",
			payload:      `{"request":{"model":"m","contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"` + bypass + `"}]}]}}`,
		},
		{
			name:         "nested_contents_path_missing",
			contentsPath: "request.contents",
			payload:      `{"request":{"model":"m"}}`,
		},
		{
			name: "pretty_printed",
			payload: `{
  "contents": [
    {
      "role": "model",
      "parts": [
        {
          "functionCall": {"name": "f", "args": {}},
          "thoughtSignature": "` + bypass + `"
        },
        {
          "text": "done"
        }
      ]
    },
    {
      "role": "user",
      "parts": [
        {
          "functionResponse": {
            "name": "f",
            "response": {"result": "ok"},
            "thought_signature": "bad"
          }
        }
      ]
    }
  ]
}`,
		},
	}

	cases = append(cases,
		geminiSanitizeCase{name: "conversation_20_turns", payload: buildGeminiConversation(20, 256, "contents")},
		geminiSanitizeCase{name: "conversation_40_turns_nested", contentsPath: "request.contents", payload: buildGeminiConversation(40, 512, "request.contents")},
	)
	return cases
}

// buildGeminiConversation renders a multi-turn Gemini request with model turns
// that carry thought and functionCall signatures plus matching functionResponse
// turns. contentsPath selects between a plain and a wrapped request shape.
func buildGeminiConversation(turns, textSize int, contentsPath string) string {
	filler := strings.Repeat("lorem ipsum dolor sit amet ", (textSize/27)+1)[:textSize]

	var b strings.Builder
	b.Grow(turns * textSize * 4)
	if contentsPath == "request.contents" {
		b.WriteString(`{"project":"","model":"gemini-3-pro-preview","request":{`)
	} else {
		b.WriteString(`{`)
	}
	b.WriteString(`"systemInstruction":{"parts":[{"text":"` + filler + `"}]},"contents":[`)
	for i := 0; i < turns; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		switch i % 4 {
		case 0:
			fmt.Fprintf(&b, `{"role":"user","parts":[{"text":"turn %d %s"}]}`, i, filler)
		case 1:
			fmt.Fprintf(&b,
				`{"role":"model","parts":[{"text":"thinking %d %s","thought":true,"thoughtSignature":"%s"},{"functionCall":{"name":"read_file","args":{"path":"pkg/file_%d.go","limit":200}},"thoughtSignature":"%s"},{"functionCall":{"name":"grep","args":{"pattern":"func %d"}},"extra_content":{"google":{"thought_signature":"%s"}}}]}`,
				i, filler, perfCompatibleSignature("t"), i, perfBypassSignature(), i, perfBypassSignature())
		case 2:
			fmt.Fprintf(&b,
				`{"role":"user","parts":[{"functionResponse":{"name":"read_file","response":{"result":"%s"},"thoughtSignature":"leaked"}},{"functionResponse":{"name":"grep","response":{"result":"%s"}},"thought_signature":"leaked"}]}`,
				filler, filler)
		default:
			fmt.Fprintf(&b, `{"parts":[{"text":"no role %d %s","thoughtSignature":"%s"}]}`, i, filler, perfBypassSignature())
		}
	}
	b.WriteString(`],"tools":[{"function_declarations":[{"name":"read_file","description":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]}],"generationConfig":{"temperature":0.7,"thinkingConfig":{"includeThoughts":true}}`)
	if contentsPath == "request.contents" {
		b.WriteString(`}`)
	}
	b.WriteString(`}`)
	return b.String()
}

func TestSanitizeGeminiRequestThoughtSignaturesMatchesLegacy(t *testing.T) {
	byteIdentical := 0
	for _, testCase := range geminiSanitizeCorpus() {
		t.Run(testCase.name, func(t *testing.T) {
			legacyInput := []byte(testCase.payload)
			newInput := []byte(testCase.payload)

			legacyOut := legacySanitizeGeminiRequestThoughtSignatures(legacyInput, testCase.contentsPath)
			newOut := SanitizeGeminiRequestThoughtSignatures(newInput, testCase.contentsPath)

			if string(newInput) != testCase.payload {
				t.Fatalf("caller buffer was modified in place")
			}

			var legacyValue, newValue any
			if err := json.Unmarshal(legacyOut, &legacyValue); err != nil {
				t.Fatalf("legacy output is not valid JSON: %v", err)
			}
			if err := json.Unmarshal(newOut, &newValue); err != nil {
				t.Fatalf("new output is not valid JSON: %v", err)
			}
			if !reflect.DeepEqual(legacyValue, newValue) {
				t.Fatalf("JSON mismatch\nlegacy: %s\nnew:    %s", legacyOut, newOut)
			}
			if string(legacyOut) == string(newOut) {
				byteIdentical++
			} else {
				t.Logf("semantically equal but not byte-identical\nlegacy: %s\nnew:    %s", legacyOut, newOut)
			}
		})
	}
	t.Logf("byte-identical cases: %d/%d", byteIdentical, len(geminiSanitizeCorpus()))
}

func TestSanitizeGeminiRequestThoughtSignaturesByteIdenticalToLegacy(t *testing.T) {
	for _, testCase := range geminiSanitizeCorpus() {
		legacyOut := legacySanitizeGeminiRequestThoughtSignatures([]byte(testCase.payload), testCase.contentsPath)
		newOut := SanitizeGeminiRequestThoughtSignatures([]byte(testCase.payload), testCase.contentsPath)
		if string(legacyOut) != string(newOut) {
			t.Errorf("%s: output is not byte-identical\nlegacy: %s\nnew:    %s", testCase.name, legacyOut, newOut)
		}
	}
}

func TestApplyGeminiPartEditsFallbackMatchesSplice(t *testing.T) {
	payload := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"bad"}]}]}`)
	edits := []geminiPartEdit{{
		start: 0, // invalid offset forces the path-based fallback
		end:   0,
		path:  "contents.0.parts.0",
		raw:   []byte(`{"functionCall":{"name":"f","args":{}},"thoughtSignature":"ok"}`),
	}}
	got := applyGeminiPartEdits(payload, edits)
	want := `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"ok"}]}]}`
	if string(got) != want {
		t.Fatalf("fallback output = %s, want %s", got, want)
	}
}

// benchmarkGeminiPayload renders a ~300 KB, 40-turn conversation.
func benchmarkGeminiPayload(b *testing.B) []byte {
	b.Helper()
	payload := []byte(buildGeminiConversation(40, 5600, "contents"))
	b.Logf("benchmark payload size: %d bytes", len(payload))
	return payload
}

func BenchmarkSanitizeGeminiRequestThoughtSignaturesLegacy(b *testing.B) {
	payload := benchmarkGeminiPayload(b)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		input := make([]byte, len(payload))
		copy(input, payload)
		_ = legacySanitizeGeminiRequestThoughtSignatures(input, "contents")
	}
}

func BenchmarkSanitizeGeminiRequestThoughtSignaturesNew(b *testing.B) {
	payload := benchmarkGeminiPayload(b)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		input := make([]byte, len(payload))
		copy(input, payload)
		_ = SanitizeGeminiRequestThoughtSignatures(input, "contents")
	}
}
