package proxy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type links struct{ key []byte }

func loadLinks(directory string) (*links, error) {
	filename := filepath.Join(directory, "url-signing.key")
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		file, err := os.Open(filename)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		key, err := io.ReadAll(io.LimitReader(file, 33))
		if err != nil {
			return nil, err
		}
		if len(key) != 32 {
			return nil, errors.New("invalid cached URL-signing key")
		}
		return &links{key: key}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("create URL-signing key: %w", err)
	}
	key := make([]byte, 32)
	if _, err = rand.Read(key); err == nil {
		_, err = file.Write(key)
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return nil, errors.Join(err, os.Remove(filename))
	}
	return &links{key: key}, nil
}

func (l *links) sign(purpose, rawURL string) string {
	mac := hmac.New(sha256.New, l.key)
	mac.Write([]byte(purpose + "\x00" + rawURL))
	return base64.RawURLEncoding.EncodeToString([]byte(rawURL)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (l *links) resolve(purpose, token string) (string, bool) {
	if len(token) > 12000 {
		return "", false
	}
	payload, signature, ok := strings.Cut(token, ".")
	if !ok {
		return "", false
	}
	rawURL, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", false
	}
	provided, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, l.key)
	mac.Write([]byte(purpose + "\x00" + string(rawURL)))
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return "", false
	}
	// Only links discovered in upstream metadata can become fetchable routes.
	// Persisting the key keeps previously issued links valid across restarts.
	return string(rawURL), true
}
