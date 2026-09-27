package deploy

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"onegit/internal/store"
)

// Secrets are encrypted with AES-256-GCM. The key comes from
// ONEGIT_SECRET_KEY; without it a key is generated and kept in the database,
// which protects against leaked backups of the secrets table only.

func (s *Service) initKey(ctx context.Context) error {
	if k := s.Cfg.Secrets.Key; k != "" {
		sum := sha256.Sum256([]byte(k))
		s.key = sum[:]
		return nil
	}
	k, err := s.Store.Secret(ctx, "deploy_secrets_key", func() ([]byte, error) {
		b := make([]byte, 32)
		_, err := rand.Read(b)
		return b, err
	})
	if err != nil {
		return err
	}
	s.Log.Warn("ONEGIT_SECRET_KEY is not set: deploy secrets are encrypted with a key stored in the database")
	s.key = k
	return nil
}

// KeyFromEnv reports whether the encryption key is configured outside the
// database.
func (s *Service) KeyFromEnv() bool { return s.Cfg.Secrets.Key != "" }

func (s *Service) encrypt(plain string) ([]byte, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plain), nil), nil
}

func (s *Service) decrypt(box []byte) (string, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(box) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	plain, err := gcm.Open(nil, box[:gcm.NonceSize()], box[gcm.NonceSize():], nil)
	if err != nil {
		return "", errors.New("cannot decrypt (was ONEGIT_SECRET_KEY changed?)")
	}
	return string(plain), nil
}

// SaveSecret stores a secret; an empty value on update keeps the old one.
func (s *Service) SaveSecret(ctx context.Context, by *store.User, sec *store.DeploySecret, value string) error {
	if value != "" {
		box, err := s.encrypt(value)
		if err != nil {
			return err
		}
		sec.Value = box
	} else if sec.ID == 0 {
		return errors.New("a value is required")
	}
	if err := s.Store.SaveDeploySecret(ctx, sec, by.ID); err != nil {
		return err
	}
	return s.Store.Audit(ctx, &by.ID, "secret.save", sec.Name, map[string]any{"selector": sec.Selector})
}

// SecretsFor returns the secrets for a target. When several secrets share a
// name, the one whose selector names the most dimensions wins, then the
// newest.
func (s *Service) SecretsFor(ctx context.Context, target map[string]string) (map[string]string, error) {
	all, err := s.Store.ListDeploySecrets(ctx)
	if err != nil {
		return nil, err
	}
	best := map[string]*store.DeploySecret{}
	for _, sec := range all {
		if !selectorMatches(sec.Selector, target) {
			continue
		}
		cur := best[sec.Name]
		if cur == nil || specificity(sec.Selector) > specificity(cur.Selector) ||
			(specificity(sec.Selector) == specificity(cur.Selector) && sec.ID > cur.ID) {
			best[sec.Name] = sec
		}
	}
	out := make(map[string]string, len(best))
	for name, sec := range best {
		v, err := s.decrypt(sec.Value)
		if err != nil {
			return nil, fmt.Errorf("secret %s: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}

func specificity(sel store.Selector) int {
	n := 0
	for _, pats := range sel {
		if len(pats) > 0 {
			n++
		}
	}
	return n
}
