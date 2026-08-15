package executor

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type repeatedByteReader byte

func (r repeatedByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

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

func TestMultipartTemporaryFileErrorsRemainServerErrors(t *testing.T) {
	tempPath := filepath.Join(t.TempDir(), "not-a-directory")
	if errWrite := os.WriteFile(tempPath, []byte("block temp files"), 0o600); errWrite != nil {
		t.Fatalf("create TMPDIR blocker: %v", errWrite)
	}
	t.Setenv("TMPDIR", tempPath)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, errPart := writer.CreateFormFile("image", "large.png")
	if errPart != nil {
		t.Fatalf("create multipart file: %v", errPart)
	}
	if _, errCopy := io.CopyN(part, repeatedByteReader('x'), openAICompatMultipartMemory+1); errCopy != nil {
		t.Fatalf("write multipart file: %v", errCopy)
	}
	contentType := writer.FormDataContentType()
	boundary := writer.Boundary()
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "openai compat",
			call: func() error {
				_, _, err := rewriteOpenAICompatImagesMultipartPayload(body.Bytes(), "gpt-image-1", boundary, false)
				return err
			},
		},
		{
			name: "codex direct",
			call: func() error {
				_, _, err := codexRewriteOpenAIImageEditMultipartToJSON(body.Bytes(), "gpt-image-1", boundary, false)
				return err
			},
		},
		{
			name: "codex responses",
			call: func() error {
				_, err := codexPrepareOpenAIImageEditMultipart(body.Bytes(), "gpt-image-1", contentType)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if err == nil {
				t.Fatal("multipart parse error = nil, want temporary-file failure")
			}
			var pathErr *os.PathError
			if !errors.As(err, &pathErr) {
				t.Fatalf("error %T does not preserve the temporary-file PathError: %v", err, err)
			}
			if statusErr, ok := err.(cliproxyexecutor.StatusError); ok && statusErr.StatusCode() == http.StatusBadRequest {
				t.Fatalf("temporary-file failure reported as HTTP 400: %v", err)
			}
		})
	}
}
