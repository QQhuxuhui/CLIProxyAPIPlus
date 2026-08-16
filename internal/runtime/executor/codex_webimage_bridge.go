package executor

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/webimage"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

type codexWebImageGenerator interface {
	Generate(context.Context, webimage.Credentials, string) ([]webimage.ImageResult, *webimage.Meta, error)
}

func isCodexWebImageRequest(opts cliproxyexecutor.Options) bool {
	if !strings.EqualFold(strings.TrimSpace(opts.SourceFormat.String()), webimage.SourceFormat) {
		return false
	}
	path := strings.TrimSpace(helps.PayloadRequestPath(opts))
	return path == codexImagesGenerationsPath || strings.HasSuffix(path, codexImagesGenerationsPath)
}

func (e *CodexAutoExecutor) executeWebImage(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.webImageExec == nil {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusServiceUnavailable, msg: "web image executor is not configured"}
	}
	payload := req.Payload
	if len(opts.OriginalRequest) > 0 {
		payload = opts.OriginalRequest
	}
	prompt := strings.TrimSpace(gjson.GetBytes(payload, "prompt").String())
	if prompt == "" {
		return cliproxyexecutor.Response{}, badRequestErr(fmt.Errorf("prompt is required"))
	}

	accessToken, _ := codexCreds(auth)
	credentials := webimage.Credentials{AccessToken: accessToken}
	if auth != nil {
		credentials.AuthID = auth.ID
		credentials.ProxyURL = auth.ProxyURL
		if auth.Metadata != nil {
			credentials.AccountID, _ = auth.Metadata["account_id"].(string)
		}
	}
	results, meta, errGenerate := e.webImageExec.Generate(ctx, credentials, prompt)
	if errGenerate != nil {
		return cliproxyexecutor.Response{}, errGenerate
	}
	if len(results) == 0 {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadGateway, msg: "web image upstream returned no image"}
	}

	createdAt := time.Now().Unix()
	if meta != nil && meta.CreatedAt > 0 {
		createdAt = meta.CreatedAt
	}
	converted := make([]codexImageCallResult, 0, len(results))
	for _, result := range results {
		converted = append(converted, codexImageCallResult{
			Result:        result.Base64Data,
			RevisedPrompt: result.RevisedPrompt,
			OutputFormat:  result.OutputFormat,
			Size:          "1024x1024",
		})
	}
	firstMeta := converted[0]
	response, errBuild := codexBuildImagesAPIResponse(converted, createdAt, nil, firstMeta, "b64_json")
	if errBuild != nil {
		return cliproxyexecutor.Response{}, errBuild
	}
	return cliproxyexecutor.Response{Payload: response}, nil
}
