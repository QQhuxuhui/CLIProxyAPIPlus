package executor

import (
	"net/http"
	"strings"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// Client-request faults must reach the conductor carrying HTTP 400 and the
// invalid_request_error marker.
//
// Without a status code they land in MarkResult's case 0 branch, which exists to
// bench credentials on transport faults, and isRequestInvalidError's default arm
// returns false so the conductor moves on to the next credential and repeats the
// whole thing. With max-retry-credentials unset (the default) one malformed
// request can therefore walk the entire pool and leave a 60s cooldown on every
// account it touches -- while never getting any closer to succeeding, since the
// same bytes fail identically against every credential.
//
// The 400 keeps them out of case 0 entirely, and the marker is what
// isRequestInvalidError (sdk/cliproxy/auth/conductor.go) matches on to stop the
// retry loop at the first credential.
func TestClientRequestFaultsCarryBadRequestStatus(t *testing.T) {
	const malformedMultipart = "--nope\r\nnot really a multipart body\r\n"

	cases := []struct {
		name string
		err  error
	}{
		{
			name: "imagen prompt missing",
			err: func() error {
				_, err := convertToImagenRequest([]byte(`{"contents":[{"parts":[{"inline_data":{}}]}]}`))
				return err
			}(),
		},
		{
			name: "openai compat multipart boundary missing",
			err: func() error {
				_, _, err := prepareOpenAICompatImagesPayload([]byte(malformedMultipart), "gpt-image-1", "multipart/form-data", false)
				return err
			}(),
		},
		{
			name: "codex multipart boundary missing",
			err: func() error {
				_, _, err := codexPrepareDirectOpenAIImageEditPayload([]byte(malformedMultipart), "gpt-image-1", "multipart/form-data", false)
				return err
			}(),
		},
		{
			name: "codex generation json invalid",
			err: func() error {
				_, err := codexPrepareOpenAIImageGenerationJSON([]byte(`not json at all`), "gpt-image-1")
				return err
			}(),
		},
		{
			name: "codex edit json invalid",
			err: func() error {
				_, err := codexPrepareOpenAIImageEditJSON([]byte(`not json at all`), "gpt-image-1")
				return err
			}(),
		},
		{
			name: "codex edit multipart content type unparseable",
			err: func() error {
				_, err := codexPrepareOpenAIImageEditMultipart([]byte(malformedMultipart), "gpt-image-1", "multipart/form-data; boundary=")
				return err
			}(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err == nil {
				t.Fatal("expected an error")
			}
			statusError, ok := tc.err.(cliproxyexecutor.StatusError)
			if !ok {
				t.Fatalf("error %T does not expose StatusCode(); it would reach MarkResult as status 0 and bench the credential", tc.err)
			}
			if got := statusError.StatusCode(); got != http.StatusBadRequest {
				t.Fatalf("StatusCode() = %d, want %d", got, http.StatusBadRequest)
			}
			if !strings.Contains(tc.err.Error(), "invalid_request_error") {
				t.Fatalf("error %q lacks the invalid_request_error marker isRequestInvalidError matches on", tc.err.Error())
			}
		})
	}
}

// badRequestErr must not turn a nil error into a non-nil one, otherwise wrapping
// a success path would synthesise a failure.
func TestBadRequestErrPassesNilThrough(t *testing.T) {
	if err := badRequestErr(nil); err != nil {
		t.Fatalf("badRequestErr(nil) = %v, want nil", err)
	}
}
