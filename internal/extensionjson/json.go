// Package extensionjson holds the bounded JSON and exact-number policy for extensions.
package extensionjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Validate rejects oversized, non-UTF-8, duplicate-key, deep or trailing JSON.
func Validate(data []byte, maxBytes int) error {
	if len(data) > maxBytes {
		return errors.New("JSON exceeds byte limit")
	}
	if !utf8.Valid(data) {
		return errors.New("JSON must be UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := uniqueValue(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func uniqueValue(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("JSON nesting exceeds 64")
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			t, e := d.Token()
			if e != nil {
				return e
			}
			k, ok := t.(string)
			if !ok || keys[k] {
				return fmt.Errorf("duplicate or invalid key %q", k)
			}
			keys[k] = true
			if e = uniqueValue(d, depth+1); e != nil {
				return e
			}
		}
	case '[':
		for d.More() {
			if e := uniqueValue(d, depth+1); e != nil {
				return e
			}
		}
	default:
		return errors.New("unexpected delimiter")
	}
	_, err = d.Token()
	return err
}

// Number parses bounded decimal text exactly, without binary floating point.
// Its argument must already be a valid JSON number from a JSON decoder.
func Number(n json.Number) (*big.Rat, bool) {
	if len(n) > 128 {
		return nil, false
	}
	if i := strings.IndexAny(string(n), "eE"); i >= 0 {
		e, err := strconv.Atoi(string(n)[i+1:])
		if err != nil || e > 308 || e < -308 {
			return nil, false
		}
	}
	return new(big.Rat).SetString(string(n))
}
