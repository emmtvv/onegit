// Package kv wraps Redis: sessions, caches and rate limits live here so that
// onegit replicas keep no in-process state.
package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultPrefix namespaces every key, so several onegit installations (or
// test runs) can share one Redis database.
const DefaultPrefix = "onegit:"

type KV struct {
	rdb    *redis.Client
	prefix string
}

// Open connects to Redis; keys get prefix (DefaultPrefix when empty).
func Open(ctx context.Context, url, prefix string) (*KV, error) {
	if prefix == "" {
		prefix = DefaultPrefix
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	rdb := redis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("connect to redis: %w", err)
	}
	return &KV{rdb: rdb, prefix: prefix}, nil
}

func (k *KV) Close() error                   { return k.rdb.Close() }
func (k *KV) Ping(ctx context.Context) error { return k.rdb.Ping(ctx).Err() }

func (k *KV) Set(ctx context.Context, key, val string, ttl time.Duration) error {
	return k.rdb.Set(ctx, k.prefix+key, val, ttl).Err()
}

// Get returns ("", false, nil) when the key does not exist.
func (k *KV) Get(ctx context.Context, key string) (string, bool, error) {
	v, err := k.rdb.Get(ctx, k.prefix+key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (k *KV) Del(ctx context.Context, keys ...string) error {
	for i := range keys {
		keys[i] = k.prefix + keys[i]
	}
	return k.rdb.Del(ctx, keys...).Err()
}

func (k *KV) SetJSON(ctx context.Context, key string, v any, ttl time.Duration) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return k.Set(ctx, key, string(b), ttl)
}

// GetJSON decodes the value into v and reports whether the key existed.
func (k *KV) GetJSON(ctx context.Context, key string, v any) (bool, error) {
	s, ok, err := k.Get(ctx, key)
	if !ok || err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(s), v)
}

// Hit counts an event in a fixed window and reports whether the count is
// still within limit.
func (k *KV) Hit(ctx context.Context, key string, limit int64, window time.Duration) (bool, error) {
	key = k.prefix + "rl:" + key
	pipe := k.rdb.TxPipeline()
	incr := pipe.Incr(ctx, key)
	pipe.ExpireNX(ctx, key, window)
	if _, err := pipe.Exec(ctx); err != nil {
		return false, err
	}
	return incr.Val() <= limit, nil
}

// Allowed checks a counter without incrementing it.
func (k *KV) Allowed(ctx context.Context, key string, limit int64) (bool, error) {
	n, err := k.rdb.Get(ctx, k.prefix+"rl:"+key).Int64()
	if errors.Is(err, redis.Nil) {
		return true, nil
	}
	return n < limit, err
}

func (k *KV) ResetHits(ctx context.Context, key string) error {
	return k.rdb.Del(ctx, k.prefix+"rl:"+key).Err()
}
