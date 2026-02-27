package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	kiroauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// DoKiroLogin triggers the Kiro authentication flow with Google OAuth.
// This is the default login method (same as --kiro-google-login).
//
// Parameters:
//   - cfg: The application configuration
//   - options: Login options including Prompt field
func DoKiroLogin(cfg *config.Config, options *LoginOptions) {
	// Use Google login as default
	DoKiroGoogleLogin(cfg, options)
}

// DoKiroGoogleLogin triggers Kiro authentication with Google OAuth.
// This uses a custom protocol handler (kiro://) to receive the callback.
//
// Parameters:
//   - cfg: The application configuration
//   - options: Login options including prompts
func DoKiroGoogleLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}

	// Note: Kiro defaults to incognito mode for multi-account support.
	// Users can override with --no-incognito if they want to use existing browser sessions.

	manager := newAuthManager()

	// Use KiroAuthenticator with Google login
	authenticator := sdkAuth.NewKiroAuthenticator()
	record, err := authenticator.LoginWithGoogle(context.Background(), cfg, &sdkAuth.LoginOptions{
		NoBrowser: options.NoBrowser,
		Metadata:  map[string]string{},
		Prompt:    options.Prompt,
	})
	if err != nil {
		log.Errorf("Kiro Google authentication failed: %v", err)
		fmt.Println("\nTroubleshooting:")
		fmt.Println("1. Make sure the protocol handler is installed")
		fmt.Println("2. Complete the Google login in the browser")
		fmt.Println("3. If callback fails, try: --kiro-import (after logging in via Kiro IDE)")
		return
	}

	// Save the auth record
	savedPath, err := manager.SaveAuth(record, cfg)
	if err != nil {
		log.Errorf("Failed to save auth: %v", err)
		return
	}

	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	if record != nil && record.Label != "" {
		fmt.Printf("Authenticated as %s\n", record.Label)
	}
	fmt.Println("Kiro Google authentication successful!")
}

// DoKiroAWSLogin triggers Kiro authentication with AWS Builder ID.
// This uses the device code flow for AWS SSO OIDC authentication.
//
// Parameters:
//   - cfg: The application configuration
//   - options: Login options including prompts
func DoKiroAWSLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}

	// Note: Kiro defaults to incognito mode for multi-account support.
	// Users can override with --no-incognito if they want to use existing browser sessions.

	manager := newAuthManager()

	// Use KiroAuthenticator with AWS Builder ID login (device code flow)
	authenticator := sdkAuth.NewKiroAuthenticator()
	record, err := authenticator.Login(context.Background(), cfg, &sdkAuth.LoginOptions{
		NoBrowser: options.NoBrowser,
		Metadata:  map[string]string{},
		Prompt:    options.Prompt,
	})
	if err != nil {
		log.Errorf("Kiro AWS authentication failed: %v", err)
		fmt.Println("\nTroubleshooting:")
		fmt.Println("1. Make sure you have an AWS Builder ID")
		fmt.Println("2. Complete the authorization in the browser")
		fmt.Println("3. If callback fails, try: --kiro-import (after logging in via Kiro IDE)")
		return
	}

	// Save the auth record
	savedPath, err := manager.SaveAuth(record, cfg)
	if err != nil {
		log.Errorf("Failed to save auth: %v", err)
		return
	}

	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	if record != nil && record.Label != "" {
		fmt.Printf("Authenticated as %s\n", record.Label)
	}
	fmt.Println("Kiro AWS authentication successful!")
}

// DoKiroAWSAuthCodeLogin triggers Kiro authentication with AWS Builder ID using authorization code flow.
// This provides a better UX than device code flow as it uses automatic browser callback.
//
// Parameters:
//   - cfg: The application configuration
//   - options: Login options including prompts
func DoKiroAWSAuthCodeLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}

	// Note: Kiro defaults to incognito mode for multi-account support.
	// Users can override with --no-incognito if they want to use existing browser sessions.

	manager := newAuthManager()

	// Use KiroAuthenticator with AWS Builder ID login (authorization code flow)
	authenticator := sdkAuth.NewKiroAuthenticator()
	record, err := authenticator.LoginWithAuthCode(context.Background(), cfg, &sdkAuth.LoginOptions{
		NoBrowser: options.NoBrowser,
		Metadata:  map[string]string{},
		Prompt:    options.Prompt,
	})
	if err != nil {
		log.Errorf("Kiro AWS authentication (auth code) failed: %v", err)
		fmt.Println("\nTroubleshooting:")
		fmt.Println("1. Make sure you have an AWS Builder ID")
		fmt.Println("2. Complete the authorization in the browser")
		fmt.Println("3. If callback fails, try: --kiro-aws-login (device code flow)")
		return
	}

	// Save the auth record
	savedPath, err := manager.SaveAuth(record, cfg)
	if err != nil {
		log.Errorf("Failed to save auth: %v", err)
		return
	}

	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	if record != nil && record.Label != "" {
		fmt.Printf("Authenticated as %s\n", record.Label)
	}
	fmt.Println("Kiro AWS authentication successful!")
}

// DoKiroImport imports Kiro token from Kiro IDE's token file.
// This is useful for users who have already logged in via Kiro IDE
// and want to use the same credentials in CLI Proxy API.
//
// Parameters:
//   - cfg: The application configuration
//   - options: Login options (currently unused for import)
func DoKiroImport(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}

	manager := newAuthManager()

	// Use ImportFromKiroIDE instead of Login
	authenticator := sdkAuth.NewKiroAuthenticator()
	record, err := authenticator.ImportFromKiroIDE(context.Background(), cfg)
	if err != nil {
		log.Errorf("Kiro token import failed: %v", err)
		fmt.Println("\nMake sure you have logged in to Kiro IDE first:")
		fmt.Println("1. Open Kiro IDE")
		fmt.Println("2. Click 'Sign in with Google' (or GitHub)")
		fmt.Println("3. Complete the login process")
		fmt.Println("4. Run this command again")
		return
	}

	// Save the imported auth record
	savedPath, err := manager.SaveAuth(record, cfg)
	if err != nil {
		log.Errorf("Failed to save auth: %v", err)
		return
	}

	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	if record != nil && record.Label != "" {
		fmt.Printf("Imported as %s\n", record.Label)
	}
	fmt.Println("Kiro token import successful!")
}

// DoKiroJsonImport imports Kiro credentials from a JSON file containing an array of accounts.
// Each account is validated, refreshed, and saved as an individual auth file.
//
// JSON format (Social): [{"refreshToken": "aorxxx", "provider": "Google"}]
// JSON format (IdC):    [{"refreshToken": "aorxxx", "clientId": "xxx", "clientSecret": "xxx", "provider": "BuilderId"}]
//
// Parameters:
//   - cfg: The application configuration
//   - jsonFilePath: Path to the JSON file containing account credentials
func DoKiroJsonImport(cfg *config.Config, jsonFilePath string) {
	data, err := os.ReadFile(jsonFilePath)
	if err != nil {
		log.Errorf("Failed to read JSON file: %v", err)
		fmt.Printf("\nCannot read file: %s\n", jsonFilePath)
		return
	}

	var items []kiroauth.KiroJSONImportItem
	if err := json.Unmarshal(data, &items); err != nil {
		log.Errorf("Failed to parse JSON: %v", err)
		fmt.Println("\nJSON format should be an array of objects:")
		fmt.Println(`  Social: [{"refreshToken": "aorxxx", "provider": "Google"}]`)
		fmt.Println(`  IdC:    [{"refreshToken": "aorxxx", "clientId": "xxx", "clientSecret": "xxx", "provider": "BuilderId"}]`)
		return
	}

	if len(items) == 0 {
		fmt.Println("No accounts found in JSON file.")
		return
	}

	fmt.Printf("Found %d account(s) in %s\n\n", len(items), filepath.Base(jsonFilePath))

	manager := newAuthManager()
	ctx := context.Background()
	successCount := 0

	for i, item := range items {
		fmt.Printf("[%d/%d] Processing account...\n", i+1, len(items))

		validation, errValidate := kiroauth.ValidateKiroJSONImportItem(item, i)
		if errValidate != nil {
			fmt.Printf("  ✗ Skipped: %v\n", errValidate)
			continue
		}
		item = validation.Item
		accountType := validation.AccountType
		provider := validation.Provider

		// Refresh token to validate and get access_token/email
		ssoClient := kiroauth.NewSSOOIDCClient(cfg)
		var tokenData *kiroauth.KiroTokenData

		if accountType == "idc" {
			region := item.Region
			if region == "" {
				region = "us-east-1"
			}
			tokenData, err = ssoClient.RefreshTokenWithRegion(ctx, item.ClientID, item.ClientSecret, item.RefreshToken, region, "")
			if err != nil {
				tokenData, err = ssoClient.RefreshToken(ctx, item.ClientID, item.ClientSecret, item.RefreshToken)
			}
		} else {
			oauth := kiroauth.NewKiroOAuth(cfg)
			tokenData, err = oauth.RefreshToken(ctx, item.RefreshToken)
		}

		if err != nil {
			fmt.Printf("  ✗ Failed: token refresh error: %v\n", err)
			continue
		}

		email := tokenData.Email
		if email == "" {
			email = kiroauth.ExtractEmailFromJWT(tokenData.AccessToken)
		}

		// Build auth record
		expiresAt, errParse := time.Parse(time.RFC3339, tokenData.ExpiresAt)
		if errParse != nil {
			expiresAt = time.Now().Add(1 * time.Hour)
		}

		idPart := kiroauth.SanitizeEmailForFilename(email)
		if idPart == "" {
			idPart = fmt.Sprintf("%d", time.Now().UnixNano()%100000)
		}

		now := time.Now()
		var authMethod, label string
		if accountType == "idc" {
			authMethod = "builder-id"
			if strings.EqualFold(provider, "Enterprise") {
				authMethod = "idc"
			}
			label = "kiro-idc"
		} else {
			authMethod = "social"
			label = fmt.Sprintf("kiro-%s", strings.ToLower(provider))
		}

		fileName := fmt.Sprintf("%s-%s.json", label, idPart)

		metadata := map[string]any{
			"type":          "kiro",
			"access_token":  tokenData.AccessToken,
			"refresh_token": tokenData.RefreshToken,
			"profile_arn":   tokenData.ProfileArn,
			"expires_at":    tokenData.ExpiresAt,
			"auth_method":   authMethod,
			"provider":      provider,
			"email":         email,
			"last_refresh":  now.Format(time.RFC3339),
		}

		if accountType == "idc" {
			metadata["client_id"] = item.ClientID
			metadata["client_secret"] = item.ClientSecret
			if item.Region != "" {
				metadata["region"] = item.Region
			}
		}

		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "kiro",
			FileName: fileName,
			Label:    label,
			Status:   coreauth.StatusActive,
			Metadata: metadata,
			Attributes: map[string]string{
				"profile_arn": tokenData.ProfileArn,
				"source":      "json-import",
				"email":       email,
			},
			CreatedAt:        now,
			UpdatedAt:        now,
			NextRefreshAfter: expiresAt.Add(-20 * time.Minute),
		}

		savedPath, errSave := manager.SaveAuth(record, cfg)
		if errSave != nil {
			fmt.Printf("  ✗ Failed to save: %v\n", errSave)
			continue
		}

		successCount++
		if email != "" {
			fmt.Printf("  ✓ Saved: %s (Account: %s)\n", filepath.Base(savedPath), email)
		} else {
			fmt.Printf("  ✓ Saved: %s\n", filepath.Base(savedPath))
		}
	}

	fmt.Printf("\nImport complete: %d/%d accounts imported successfully.\n", successCount, len(items))
}
