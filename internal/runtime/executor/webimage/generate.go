package webimage

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const defaultBaseURL = "https://chatgpt.com"

type browserVectorFactory func(*Session) BrowserVector

// Option customizes a web image executor. Production callers use defaults;
// tests override only the upstream URL and deterministic browser vector.
type Option func(*Executor)

func WithBaseURL(baseURL string) Option {
	return func(executor *Executor) {
		if value := strings.TrimSpace(baseURL); value != "" {
			executor.baseURL = strings.TrimSuffix(value, "/")
		}
	}
}

func WithBrowserVectorFactory(factory func(*Session) BrowserVector) Option {
	return func(executor *Executor) {
		if factory != nil {
			executor.browserVector = factory
		}
	}
}

// Executor runs the isolated ChatGPT web image protocol.
type Executor struct {
	cfg           *config.Config
	baseURL       string
	sessions      *SessionPool
	browserVector browserVectorFactory
	globalSlots   chan struct{}

	accountMu    sync.Mutex
	accountSlots map[string]chan struct{}
	accountRefs  map[string]int
}

func NewExecutor(cfg *config.Config, options ...Option) *Executor {
	globalLimit := config.DefaultWebImageMaxConcurrency
	if cfg != nil && cfg.WebImageMaxConcurrency > 0 {
		globalLimit = cfg.WebImageMaxConcurrency
	}
	executor := &Executor{
		cfg:           cfg,
		baseURL:       defaultBaseURL,
		sessions:      NewSessionPool(cfg),
		browserVector: defaultBrowserVector,
		globalSlots:   make(chan struct{}, globalLimit),
		accountSlots:  make(map[string]chan struct{}),
		accountRefs:   make(map[string]int),
	}
	for _, option := range options {
		option(executor)
	}
	return executor
}

func (e *Executor) Close() {
	if e != nil && e.sessions != nil {
		e.sessions.Close()
	}
}

// Generate creates one 1K image from a prompt.
func (e *Executor) Generate(ctx context.Context, credentials Credentials, prompt string) ([]ImageResult, *Meta, error) {
	if e == nil || e.cfg == nil {
		return nil, nil, &StatusError{Status: 503, Kind: ErrorKindProtocol, Stage: "config", Msg: "web image generation is not configured"}
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, nil, &StatusError{Status: 400, Kind: ErrorKindProtocol, Stage: "request", Msg: "web image prompt is empty"}
	}
	if strings.TrimSpace(credentials.AccessToken) == "" {
		return nil, nil, &StatusError{Status: 503, Kind: ErrorKindAuth, Stage: "auth", Msg: "web image credential has no access token"}
	}
	release, errAcquire := e.acquire(ctx, credentials.AuthID)
	if errAcquire != nil {
		return nil, nil, errAcquire
	}
	defer release()

	startedAt := time.Now()
	totalDeadline := startedAt.Add(e.duration(e.cfg.WebImageTotalDeadline, config.DefaultWebImageTotalDeadline))
	session, errSession := e.sessions.Get(credentials)
	if errSession != nil {
		return nil, nil, &StatusError{Status: 503, Kind: ErrorKindChallenge, Stage: "session", Msg: "web image browser session setup failed"}
	}
	state := generationState{prompt: prompt, turnTraceID: newTurnTraceID()}
	if errBootstrap := e.bootstrap(ctx, session, credentials, &state, totalDeadline); errBootstrap != nil {
		return nil, nil, errBootstrap
	}
	if errRequirements := e.prepareRequirements(ctx, session, credentials, &state, totalDeadline); errRequirements != nil {
		return nil, nil, errRequirements
	}
	if errConversation := e.startConversation(ctx, session, credentials, &state, totalDeadline); errConversation != nil {
		return nil, nil, errConversation
	}
	if errPoll := e.pollConversation(ctx, session, credentials, &state, totalDeadline); errPoll != nil {
		return nil, nil, errPoll
	}
	imageData, outputFormat, errDownload := e.resolveAndDownload(ctx, session, credentials, &state, totalDeadline)
	if errDownload != nil {
		return nil, nil, errDownload
	}
	return []ImageResult{{Base64Data: imageData, OutputFormat: outputFormat}}, &Meta{CreatedAt: time.Now().Unix()}, nil
}

func (e *Executor) duration(value, fallback string) time.Duration {
	duration, errParse := time.ParseDuration(strings.TrimSpace(value))
	if errParse == nil && duration > 0 {
		return duration
	}
	duration, _ = time.ParseDuration(fallback)
	return duration
}

func (e *Executor) acquire(ctx context.Context, authID string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	authID = strings.TrimSpace(authID)
	e.accountMu.Lock()
	accountSlots := e.accountSlots[authID]
	if accountSlots == nil {
		limit := config.DefaultWebImageMaxConcurrencyPerAccount
		if e.cfg != nil && e.cfg.WebImageMaxConcurrencyPerAccount > 0 {
			limit = e.cfg.WebImageMaxConcurrencyPerAccount
		}
		accountSlots = make(chan struct{}, limit)
		e.accountSlots[authID] = accountSlots
	}
	e.accountRefs[authID]++
	e.accountMu.Unlock()

	select {
	case accountSlots <- struct{}{}:
	case <-ctx.Done():
		e.releaseAccountReference(authID, accountSlots)
		return nil, ctx.Err()
	}

	select {
	case e.globalSlots <- struct{}{}:
		return func() {
			<-e.globalSlots
			<-accountSlots
			e.releaseAccountReference(authID, accountSlots)
		}, nil
	case <-ctx.Done():
		<-accountSlots
		e.releaseAccountReference(authID, accountSlots)
		return nil, ctx.Err()
	}
}

func (e *Executor) releaseAccountReference(authID string, accountSlots chan struct{}) {
	e.accountMu.Lock()
	defer e.accountMu.Unlock()

	remaining := e.accountRefs[authID] - 1
	if remaining <= 0 && e.accountSlots[authID] == accountSlots {
		delete(e.accountSlots, authID)
		delete(e.accountRefs, authID)
		return
	}
	e.accountRefs[authID] = remaining
}

func (e *Executor) checkBudget(ctx context.Context, deadline time.Time, stage string) error {
	if ctx != nil {
		if errContext := ctx.Err(); errContext != nil {
			return errContext
		}
	}
	if !deadline.IsZero() && time.Now().After(deadline) {
		return &StatusError{Status: 504, Kind: ErrorKindTimeout, Stage: stage, Msg: fmt.Sprintf("web image %s budget expired", stage)}
	}
	return nil
}

func defaultBrowserVector(session *Session) BrowserVector {
	now := time.Now()
	userAgent := config.DefaultWebImageUserAgent
	clientVersion := ""
	sessionID := ""
	timeOrigin := now
	if session != nil {
		userAgent = session.UserAgent
		clientVersion = session.ClientVersion
		sessionID = session.Identity.SessionID
		if !session.createdAt.IsZero() {
			timeOrigin = session.createdAt
		}
	}
	return BrowserVector{
		3000,
		javascriptDateString(now),
		4294705152,
		0,
		userAgent,
		"https://chatgpt.com/sentinel/sdk.js",
		clientVersion,
		"en-US",
		"en-US,en",
		0,
		"vendor−Google Inc.",
		"body",
		"window",
		float64(now.Sub(timeOrigin)) / float64(time.Millisecond),
		sessionID,
		"",
		8,
		float64(timeOrigin.UnixMilli()),
		0, 0, 0, 0, 0, 0, 0,
	}
}

func javascriptDateString(value time.Time) string {
	chinaStandardTime := time.FixedZone("China Standard Time", 8*60*60)
	return value.In(chinaStandardTime).Format("Mon Jan 02 2006 15:04:05 GMT-0700 (China Standard Time)")
}
