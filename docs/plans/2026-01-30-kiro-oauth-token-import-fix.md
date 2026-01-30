# Kiro OAuth Token Import Filename Bug Fix

**Date:** 2026-01-30
**Status:** Implemented
**Type:** Bug Fix

## Problem Statement

When importing Kiro OAuth tokens through the web UI's token import feature, multiple accounts would overwrite each other because all imported tokens were saved with the same filename `kiro-social.json`. This prevented users from managing multiple Kiro accounts simultaneously.

### User Impact

- **Before Fix:** User imports token for account A → saved as `kiro-social.json`
- **Before Fix:** User imports token for account B → overwrites `kiro-social.json` (account A is lost)
- **After Fix:** User imports token for account A → saved as `kiro-social-userA-example-com.json`
- **After Fix:** User imports token for account B → saved as `kiro-social-userB-example-com.json` (both accounts coexist)

## Root Cause Analysis

The bug occurred in the `handleImportToken` function in `internal/auth/kiro/oauth_web.go`:

1. When a token is imported via the web UI, the `RefreshSocialToken` method is called to validate the token
2. `RefreshSocialToken` returns `KiroTokenData` but **does not extract the email** from the JWT access token
3. The `tokenData.Email` field remains empty
4. When `saveTokenToFile` generates the filename, it checks if email exists:
   ```go
   fileName := fmt.Sprintf("kiro-%s.json", tokenData.AuthMethod)
   if tokenData.Email != "" {
       sanitizedEmail := strings.ReplaceAll(tokenData.Email, "@", "-")
       sanitizedEmail = strings.ReplaceAll(sanitizedEmail, ".", "-")
       fileName = fmt.Sprintf("kiro-%s-%s.json", tokenData.AuthMethod, sanitizedEmail)
   }
   ```
5. Since email is empty, all imports fall back to `kiro-social.json`

### Why Other Auth Methods Work

Other authentication methods (Builder ID, IDC, Google/GitHub OAuth flow) work correctly because they extract the email during the authentication process:

- **Builder ID/IDC:** Extract email via `FetchUserEmailWithFallback` (line 387 in oauth_web.go)
- **Social OAuth Flow:** Extract email via `ExtractEmailFromJWT` (line 597 in oauth_web.go)
- **Token Import:** ❌ Did not extract email (the bug)

## Solution

Extract the email from the JWT access token after validating the imported token, before saving to file.

### Code Changes

**File:** `internal/auth/kiro/oauth_web.go`

**Location:** `handleImportToken` function (after line 840)

**Added:**
```go
// Extract email from JWT access token for unique filename generation
if tokenData.Email == "" {
    tokenData.Email = ExtractEmailFromJWT(tokenData.AccessToken)
}
```

### How It Works

1. User imports a refresh token via the web UI
2. Token is validated by calling `RefreshSocialToken` to get a fresh access token
3. **NEW:** Email is extracted from the JWT access token using `ExtractEmailFromJWT`
4. Token is saved with email-based filename: `kiro-social-{email}.json`
5. Each account gets a unique file based on their email address

### Filename Format

- **With email:** `kiro-social-user-example-com.json`
- **Without email (fallback):** `kiro-social.json`

Email sanitization:
- `@` → `-`
- `.` → `-`

Example: `user@example.com` → `kiro-social-user-example-com.json`

## Testing

### Manual Testing Steps

1. Import first token for account A (e.g., userA@example.com)
   - Verify file created: `kiro-social-userA-example-com.json`
2. Import second token for account B (e.g., userB@example.com)
   - Verify file created: `kiro-social-userB-example-com.json`
   - Verify account A file still exists
3. Check both accounts work independently in the application

### Build Verification

```bash
make build
# Build completed successfully
```

## Consistency with Existing Code

This fix maintains consistency with other authentication methods:

- **Builder ID:** Uses email in filename (line 469: `kiro-builder-id-{email}.json`)
- **IDC:** Uses email in filename (line 469: `kiro-idc-{email}.json`)
- **Social OAuth:** Uses email in filename (line 469: `kiro-social-{email}.json`)
- **Token Import:** Now also uses email in filename ✓

## Edge Cases Handled

1. **No email in JWT:** Falls back to `kiro-social.json` (same as before)
2. **Email already set:** Skips extraction (respects existing value)
3. **Invalid JWT format:** `ExtractEmailFromJWT` returns empty string, falls back to default filename

## Related Files

- `internal/auth/kiro/oauth_web.go` - Main fix location
- `internal/auth/kiro/social_auth.go` - `RefreshSocialToken` method
- `internal/auth/kiro/aws.go` - `ExtractEmailFromJWT` utility function
- `internal/auth/kiro/token.go` - Token storage structure

## Future Improvements

Consider these enhancements in future iterations:

1. **Prompt for email if extraction fails:** Ask user to provide email/label when JWT doesn't contain email
2. **Validate email format:** Add validation to ensure extracted email is valid
3. **Conflict detection:** Warn user if importing a token that would overwrite an existing file
4. **Migration tool:** Help users rename existing `kiro-social.json` files to email-based names

## Conclusion

This fix ensures that multiple Kiro accounts can coexist by generating unique filenames based on the user's email address extracted from the JWT access token. The solution is minimal, consistent with existing code patterns, and handles edge cases gracefully.
