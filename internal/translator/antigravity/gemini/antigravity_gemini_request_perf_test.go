package gemini

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

// perfCompatibleSignature builds a Gemini 3 shaped signature that the signature
// policy preserves as-is.
func perfCompatibleSignature() string {
	payload := []byte("gemini-thought-signature")
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

// perfBypassSignature builds a base64 UUID signature that the signature policy
// replaces with the bypass sentinel.
func perfBypassSignature() string {
	return base64.StdEncoding.EncodeToString([]byte("e24830a7-5cd6-42fe-998b-ee539e72b9c3"))
}

type antigravityRequestCase struct {
	name    string
	model   string
	payload string
}

// antigravityRequestCorpus returns realistic Gemini requests as they reach
// ConvertGeminiRequestToAntigravity, covering tool grouping, signature shapes,
// role normalization, tool declarations and system instruction variants.
func antigravityRequestCorpus() []antigravityRequestCase {
	compatible := perfCompatibleSignature()
	bypass := perfBypassSignature()

	tools := `,"tools":[{"function_declarations":[` +
		`{"name":"read_file","description":"read a file","parameters":{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer"}},"required":["path"]}},` +
		`{"name":"grep","description":"search","parameters":{"type":"object","properties":{"pattern":{"type":"string"}}}},` +
		`{"name":"noop","description":"no parameters"},` +
		`{"name":"already_json_schema","parametersJsonSchema":{"type":"object"}}` +
		`]},{"function_declarations":[{"name":"write_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]}]`

	cases := []antigravityRequestCase{
		{name: "no_contents", payload: `{"model":"gemini-3-pro-preview"}`},
		{name: "empty_contents", payload: `{"model":"gemini-3-pro-preview","contents":[]}`},
		{name: "single_user_turn", payload: `{"model":"gemini-3-pro-preview","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
		{
			name:    "system_instruction_snake_case",
			payload: `{"model":"m","system_instruction":{"parts":[{"text":"be nice"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
		},
		{
			name:    "system_instruction_camel_case",
			payload: `{"model":"m","systemInstruction":{"parts":[{"text":"be nice"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
		},
		{
			name:    "tools_with_parameters",
			payload: `{"model":"m","contents":[{"role":"user","parts":[{"text":"hi"}]}]` + tools + `}`,
		},
		{
			name:    "generation_config",
			payload: `{"model":"m","contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.5,"thinkingConfig":{"includeThoughts":true,"thinkingBudget":1024}}}`,
		},
		{
			name: "single_function_call_and_response",
			payload: `{"model":"m","contents":[` +
				`{"role":"user","parts":[{"text":"read it"}]},` +
				`{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{"path":"a.go"}},"thoughtSignature":"` + bypass + `"}]},` +
				`{"role":"user","parts":[{"functionResponse":{"name":"read_file","response":{"result":"content"}}}]}` +
				`]}`,
		},
		{
			name: "parallel_function_calls",
			payload: `{"model":"m","contents":[` +
				`{"role":"model","parts":[{"functionCall":{"name":"a","args":{}}},{"functionCall":{"name":"b","args":{}}}]},` +
				`{"role":"user","parts":[{"functionResponse":{"name":"a","response":{"result":"1"}}}]},` +
				`{"role":"user","parts":[{"functionResponse":{"name":"","response":{"result":"2"}}}]}` +
				`]}`,
		},
		{
			name: "responses_without_calls",
			payload: `{"model":"m","contents":[` +
				`{"role":"user","parts":[{"functionResponse":{"name":"orphan","response":{"result":"1"}}}]}` +
				`]}`,
		},
		{
			name: "calls_without_responses",
			payload: `{"model":"m","contents":[` +
				`{"role":"model","parts":[{"functionCall":{"name":"pending","args":{}}}]}` +
				`]}`,
		},
		{
			name: "function_response_non_object_response",
			payload: `{"model":"m","contents":[` +
				`{"role":"model","parts":[{"functionCall":{"name":"a","args":{}}}]},` +
				`{"role":"user","parts":[{"functionResponse":{"name":"a","response":"plain string","id":"call_1"}}]}` +
				`]}`,
		},
		{
			name: "missing_and_invalid_roles",
			payload: `{"model":"m","contents":[` +
				`{"parts":[{"text":"first"}]},` +
				`{"role":"assistant","parts":[{"text":"second"}]},` +
				`{"role":"","parts":[{"text":"third"}]},` +
				`{"role":"user","parts":[{"text":"fourth"}]}` +
				`]}`,
		},
		{
			name: "signature_variants",
			payload: `{"model":"m","contents":[` +
				`{"role":"model","parts":[{"text":"thinking","thought":true,"thoughtSignature":"` + compatible + `"},{"functionCall":{"name":"a","args":{},"thought_signature":"` + bypass + `"}},{"functionCall":{"name":"b","args":{}},"extra_content":{"google":{"thought_signature":"` + bypass + `"}}}]},` +
				`{"role":"user","parts":[{"functionResponse":{"name":"a","response":{"result":"1"},"thoughtSignature":"leak"}},{"functionResponse":{"name":"b","response":{"result":"2"}},"thought_signature":"leak"}]}` +
				`]}`,
		},
		{
			name:  "claude_model_path",
			model: "claude-sonnet-4-5",
			payload: `{"model":"claude","contents":[` +
				`{"role":"model","parts":[{"text":"thinking","thought":true,"thoughtSignature":"` + compatible + `"},{"text":"answer"}]},` +
				`{"role":"user","parts":[{"functionResponse":{"name":"a","response":{"result":"1"},"thoughtSignature":"leak"}}]}` +
				`]}`,
		},
		{
			name:    "unicode_and_escapes",
			payload: `{"model":"m","contents":[{"role":"user","parts":[{"text":"quote \" emoji 🚀 backslash \\ newline \n"}]}]}`,
		},
		{
			name: "pretty_printed",
			payload: `{
  "model": "gemini-3-pro-preview",
  "contents": [
    {
      "role": "model",
      "parts": [
        {"functionCall": {"name": "a", "args": {}}}
      ]
    },
    {
      "role": "user",
      "parts": [
        {"functionResponse": {"name": "a", "response": {"result": "ok"}}}
      ]
    }
  ],
  "tools": [
    {
      "function_declarations": [
        {"name": "a", "parameters": {"type": "object"}}
      ]
    }
  ]
}`,
		},
		{
			name:    "scalar_content_entries",
			payload: `{"model":"m","contents":["oops",{"role":"user","parts":[{"text":"hi"}]},42]}`,
		},
		{
			name:    "content_parts_missing",
			payload: `{"model":"m","contents":[{"role":"user"},{"role":"model"}]}`,
		},
		{name: "conversation_20_turns", payload: buildAntigravityConversation(20, 256)},
		{name: "conversation_40_turns", payload: buildAntigravityConversation(40, 512)},
	}

	for i := range cases {
		if cases[i].model == "" {
			cases[i].model = "gemini-3-pro-preview"
		}
	}
	return cases
}

// buildAntigravityConversation renders a multi-turn Gemini request with tool
// call/response groups, signatures, tools and a system instruction.
func buildAntigravityConversation(turns, textSize int) string {
	filler := strings.Repeat("lorem ipsum dolor sit amet ", (textSize/27)+1)[:textSize]

	var b strings.Builder
	b.Grow(turns * textSize * 4)
	b.WriteString(`{"model":"gemini-3-pro-preview","system_instruction":{"parts":[{"text":"` + filler + `"}]},"contents":[`)
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
				i, filler, perfCompatibleSignature(), i, perfBypassSignature(), i, perfBypassSignature())
		case 2:
			fmt.Fprintf(&b,
				`{"role":"user","parts":[{"functionResponse":{"name":"read_file","response":{"result":"%s"},"thoughtSignature":"leaked"}},{"functionResponse":{"name":"","response":{"result":"%s"}},"thought_signature":"leaked"}]}`,
				filler, filler)
		default:
			fmt.Fprintf(&b, `{"parts":[{"text":"no role %d %s"}]}`, i, filler)
		}
	}
	b.WriteString(`],"tools":[{"function_declarations":[`)
	for i := 0; i < 12; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"name":"tool_%d","description":"%s","parameters":{"type":"object","properties":{"path":{"type":"string","description":"%s"},"limit":{"type":"integer"}},"required":["path"]}}`, i, filler[:64], filler[:64])
	}
	b.WriteString(`]}],"generationConfig":{"temperature":0.7,"thinkingConfig":{"includeThoughts":true}}}`)
	return b.String()
}

func TestConvertGeminiRequestToAntigravityMatchesLegacy(t *testing.T) {
	byteIdentical := 0
	corpus := antigravityRequestCorpus()
	for _, testCase := range corpus {
		t.Run(testCase.name, func(t *testing.T) {
			legacyInput := []byte(testCase.payload)
			newInput := []byte(testCase.payload)

			legacyOut := legacyConvertGeminiRequestToAntigravity(testCase.model, legacyInput, false)
			newOut := ConvertGeminiRequestToAntigravity(testCase.model, newInput, false)

			if string(newInput) != testCase.payload {
				t.Fatalf("caller buffer was modified in place")
			}
			if len(legacyOut) == 0 || len(newOut) == 0 {
				if len(legacyOut) != len(newOut) {
					t.Fatalf("empty-output mismatch: legacy %d bytes, new %d bytes", len(legacyOut), len(newOut))
				}
				byteIdentical++
				return
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
	t.Logf("byte-identical cases: %d/%d", byteIdentical, len(corpus))
}

func TestFixCLIToolResponseMatchesLegacy(t *testing.T) {
	for _, testCase := range antigravityRequestCorpus() {
		wrapped := `{"project":"","request":` + testCase.payload + `,"model":"` + testCase.model + `"}`

		legacyOut, legacyErr := legacyFixCLIToolResponse(wrapped)
		newOut, newErr := fixCLIToolResponse(wrapped)

		if (legacyErr == nil) != (newErr == nil) {
			t.Fatalf("%s: error mismatch: legacy %v, new %v", testCase.name, legacyErr, newErr)
		}
		if legacyOut != newOut {
			t.Errorf("%s: fixCLIToolResponse output is not byte-identical\nlegacy: %s\nnew:    %s", testCase.name, legacyOut, newOut)
		}
	}
}

func benchmarkAntigravityPayload(b *testing.B) []byte {
	b.Helper()
	payload := []byte(buildAntigravityConversation(40, 5400))
	b.Logf("benchmark payload size: %d bytes", len(payload))
	return payload
}

func BenchmarkConvertGeminiRequestToAntigravityLegacy(b *testing.B) {
	payload := benchmarkAntigravityPayload(b)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = legacyConvertGeminiRequestToAntigravity("gemini-3-pro-preview", payload, false)
	}
}

func BenchmarkConvertGeminiRequestToAntigravityNew(b *testing.B) {
	payload := benchmarkAntigravityPayload(b)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ConvertGeminiRequestToAntigravity("gemini-3-pro-preview", payload, false)
	}
}

func BenchmarkFixCLIToolResponseLegacy(b *testing.B) {
	payload := `{"project":"","request":` + buildAntigravityConversation(40, 5400) + `,"model":"gemini-3-pro-preview"}`
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = legacyFixCLIToolResponse(payload)
	}
}

func BenchmarkFixCLIToolResponseNew(b *testing.B) {
	payload := []byte(`{"project":"","request":` + buildAntigravityConversation(40, 5400) + `,"model":"gemini-3-pro-preview"}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		input := make([]byte, len(payload))
		copy(input, payload)
		_, _ = fixCLIToolResponseBytes(input)
	}
}
