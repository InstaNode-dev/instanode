package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

type redisCreds struct {
	url       string
	keyPrefix string
}

func provisionRedis(ctx context.Context, rdb *redis.Client, token string) (*redisCreds, error) {
	safe := sanitizeToken(token)
	userName := "usr_" + safe
	keyPrefix := safe + ":"
	password := randomRedisPassword(24)

	// Try ACL-based isolation (Redis 6+).
	// Pattern: allow only keys starting with the token prefix.
	err := rdb.Do(ctx, "ACL", "SETUSER", userName,
		"on",
		">"+password,
		"~"+keyPrefix+"*",
		"+@all",
		"-@admin",
		"-@dangerous",
	).Err()

	if err != nil {
		// ACL not supported — fall back to key-prefix-only isolation.
		// The shared connection URL is used with a key prefix convention.
		connOpts := rdb.Options()
		url := fmt.Sprintf("redis://:%s@%s/%d", connOpts.Password, connOpts.Addr, connOpts.DB)
		return &redisCreds{url: url, keyPrefix: keyPrefix}, nil
	}

	connOpts := rdb.Options()
	host := connOpts.Addr
	url := fmt.Sprintf("redis://%s:%s@%s/0", userName, password, host)
	return &redisCreds{url: url, keyPrefix: keyPrefix}, nil
}

func randomRedisPassword(length int) string {
	b := make([]byte, length)
	rand.Read(b)
	s := hex.EncodeToString(b)
	s = strings.ReplaceAll(s, "e", "f") // avoid scientific notation confusion
	return s[:length]
}
