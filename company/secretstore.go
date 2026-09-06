// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"

	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	secret_module "gitea.dev/modules/secret"
	"gitea.dev/modules/setting"
)

// Per-user secrets (AI API keys today, department app environment
// variables next) are stored encrypted at rest with the instance's
// SECRET_KEY, using the same primitive Gitea's own Actions secrets use
// (models/secret/secret.go). user_model.SetUserSetting stores whatever it
// is given verbatim — and caches it in plaintext too — so encrypting has
// to happen here, at the call site, not in the setting layer.
//
// What this protects against: another department's app reading gitea.db,
// someone with disk/backup access, and a leaked database dump. What it
// does NOT protect against: the person who typed the value in. That is a
// property of the feature, not a gap — an app that uses a key can always
// reveal it. See docs/company/app-platform.md.

// setUserSecret stores value encrypted under key for userID. An empty
// value is stored as-is: "" means "not set", and encrypting it would
// produce a non-empty ciphertext that every "is it set?" check would then
// read as configured.
func setUserSecret(ctx context.Context, userID int64, key, value string) error {
	if value == "" {
		return user_model.SetUserSetting(ctx, userID, key, "")
	}
	encrypted, err := secret_module.EncryptSecret(setting.SecretKey, value)
	if err != nil {
		return err
	}
	return user_model.SetUserSetting(ctx, userID, key, encrypted)
}

// getUserSecret returns the decrypted value stored under key.
//
// Values written before encryption landed are plaintext, and there is no
// flag distinguishing the two — so a decrypt failure is treated as "this
// is a legacy plaintext value", returned as-is, and rewritten encrypted so
// the next read is clean. This is safe to infer because DecryptSecret
// fails on anything that isn't valid hex of the right shape, which real
// API keys ("sk-...") are not.
//
// The rewrite is best-effort: if it fails the caller still gets the right
// value and the migration simply retries on the next read. A read must
// never fail because a *write* failed.
func getUserSecret(ctx context.Context, userID int64, key string) (string, error) {
	stored, err := user_model.GetUserSetting(ctx, userID, key)
	if err != nil || stored == "" {
		return "", err
	}
	decrypted, decErr := secret_module.DecryptSecret(setting.SecretKey, stored)
	if decErr == nil {
		return decrypted, nil
	}
	// Legacy plaintext — migrate it forward, then hand back what we read.
	if setErr := setUserSecret(ctx, userID, key, stored); setErr != nil {
		log.Error("company: migrating plaintext secret %q for user %d: %v", key, userID, setErr)
	}
	return stored, nil
}
