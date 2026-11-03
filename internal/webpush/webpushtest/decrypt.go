// Package webpushtest is the user agent's half of RFC 8291, for tests that read what orbit pushed.
package webpushtest

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
)

// Decrypt opens one aes128gcm push body (a single record) with the device's private key and auth secret.
func Decrypt(ua *ecdh.PrivateKey, auth, body []byte) ([]byte, error) {
	if len(body) < 21 || len(body) < 21+int(body[20]) {
		return nil, errors.New("short body")
	}
	salt, idlen := body[:16], int(body[20])
	asRaw := body[21 : 21+idlen]
	as, err := ecdh.P256().NewPublicKey(asRaw)
	if err != nil {
		return nil, err
	}
	shared, err := ua.ECDH(as)
	if err != nil {
		return nil, err
	}
	info := append(append([]byte("WebPush: info\x00"), ua.PublicKey().Bytes()...), asRaw...)
	ikm, _ := hkdf.Key(sha256.New, shared, auth, string(info), 32)
	prk, _ := hkdf.Extract(sha256.New, ikm, salt)
	cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		return nil, err
	}
	if len(plain) == 0 || plain[len(plain)-1] != 2 {
		return nil, errors.New("missing last-record delimiter")
	}
	return plain[:len(plain)-1], nil
}
