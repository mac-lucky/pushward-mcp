package e2e

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

type vectorFile struct {
	Seal []struct {
		Name      string  `json:"name"`
		KeyHex    string  `json:"key_hex"`
		EncKeyHex string  `json:"enc_key_hex"`
		KID       string  `json:"kid"`
		NonceHex  string  `json:"nonce_hex"`
		Plaintext string  `json:"plaintext"`
		Envelope  string  `json:"envelope"`
		Expect    Message `json:"expect"`
	} `json:"seal"`
	OpenFail []struct {
		Name     string `json:"name"`
		KeyHex   string `json:"key_hex"`
		Envelope string `json:"envelope"`
	} `json:"open_fail"`
	ParseFail []struct {
		Name     string `json:"name"`
		Envelope string `json:"envelope"`
	} `json:"parse_fail"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	data, err := os.ReadFile("testdata/vectors-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Seal) == 0 || len(v.OpenFail) == 0 || len(v.ParseFail) == 0 {
		t.Fatal("vector file has an empty section")
	}
	return v
}

func mustKey(t *testing.T, s string) *Key {
	t.Helper()
	k, err := ParseKey(s)
	if err != nil {
		t.Fatalf("ParseKey(%q): %v", s, err)
	}
	return k
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The sender only seals, but the vectors are checked end to end: open is the
// receiver side of the spec, kept here so a sealed envelope is proven to
// open to what the apps will show.
var errLocked = errors.New("no key for this kid")

func open(keys map[string]*Key, env string) (Message, error) {
	kid, raw, err := ParseEnvelope(env)
	if err != nil {
		return Message{}, err
	}
	k := keys[kid]
	if k == nil {
		return Message{}, errLocked
	}
	block, err := aes.NewCipher(k.enc[:])
	if err != nil {
		return Message{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Message{}, err
	}
	pt, err := gcm.Open(nil, raw[:nonceSize], raw[nonceSize:], []byte("pw1."+kid))
	if err != nil {
		return Message{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(pt, &fields); err != nil || fields == nil {
		return Message{}, errors.New("plaintext is not a JSON object")
	}
	str := func(name string, required bool) (string, error) {
		v, ok := fields[name]
		if !ok {
			if required {
				return "", errors.New(name + " missing")
			}
			return "", nil
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return "", errors.New(name + " is not a string")
		}
		if required && s == "" {
			return "", errors.New(name + " is empty")
		}
		return s, nil
	}
	var m Message
	for _, f := range []struct {
		name     string
		required bool
		dst      *string
	}{{"title", true, &m.Title}, {"body", true, &m.Body}, {"subtitle", false, &m.Subtitle}, {"url", false, &m.URL}} {
		if *f.dst, err = str(f.name, f.required); err != nil {
			return Message{}, err
		}
	}
	m.Title, m.Subtitle, m.Body = clamp(m.Title, maxTitle), clamp(m.Subtitle, maxTitle), clamp(m.Body, maxBody)
	if validateURL(m.URL) != nil {
		m.URL = ""
	}
	return m, nil
}

func clamp(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func TestVectorsSeal(t *testing.T) {
	for _, v := range loadVectors(t).Seal {
		t.Run(v.Name, func(t *testing.T) {
			k := mustKey(t, v.KeyHex)
			if k.KID() != v.KID {
				t.Errorf("kid = %s, want %s", k.KID(), v.KID)
			}
			if got := hex.EncodeToString(k.enc[:]); got != v.EncKeyHex {
				t.Errorf("enc key = %s, want %s", got, v.EncKeyHex)
			}
			env, err := k.seal(mustHex(t, v.NonceHex), []byte(v.Plaintext))
			if err != nil {
				t.Fatal(err)
			}
			if env != v.Envelope {
				t.Errorf("envelope:\n got %s\nwant %s", env, v.Envelope)
			}
			got, err := open(map[string]*Key{k.KID(): k}, v.Envelope)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if got != v.Expect {
				t.Errorf("opened %+v, want %+v", got, v.Expect)
			}
		})
	}
}

func TestVectorsOpenFail(t *testing.T) {
	for _, v := range loadVectors(t).OpenFail {
		t.Run(v.Name, func(t *testing.T) {
			k := mustKey(t, v.KeyHex)
			if m, err := open(map[string]*Key{k.KID(): k}, v.Envelope); err == nil {
				t.Errorf("opened to %+v, want a failure", m)
			}
		})
	}
}

func TestVectorsParseFail(t *testing.T) {
	for _, v := range loadVectors(t).ParseFail {
		t.Run(v.Name, func(t *testing.T) {
			if _, _, err := ParseEnvelope(v.Envelope); err == nil {
				t.Error("parsed, want a failure")
			}
		})
	}
}

// Seal itself (validation, JSON, padding, nonce from the reader) reproduces
// the two vectors whose plaintext is exactly what a sender produces: the
// padded one and the unpadded maximum.
func TestSealReproducesSenderVectors(t *testing.T) {
	for _, v := range loadVectors(t).Seal {
		if v.Name != "padded_to_64" && v.Name != "max_size" {
			continue
		}
		t.Run(v.Name, func(t *testing.T) {
			env, err := Seal(mustKey(t, v.KeyHex), v.Expect, bytes.NewReader(mustHex(t, v.NonceHex)))
			if err != nil {
				t.Fatal(err)
			}
			if env != v.Envelope {
				t.Errorf("envelope:\n got %s\nwant %s", env, v.Envelope)
			}
		})
	}
}

func TestSealRoundTripsAndPads(t *testing.T) {
	k := mustKey(t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	for _, m := range []Message{
		{Title: "Disk full", Body: "/var is at 97% on db01"},
		{Title: "Deploy failed", Subtitle: "api / production", Body: "Step 3 of 5: migrate", URL: "https://ci.example.com/runs/42"},
		{Title: "<b>&</b>", Body: "html stays as typed", URL: "myapp://incident/7"},
		{Title: strings.Repeat("\U0001F525", 100), Body: strings.Repeat("\u05ea", 500)},
	} {
		env, err := Seal(k, m, rand.Reader)
		if err != nil {
			t.Fatalf("Seal(%+v): %v", m, err)
		}
		_, raw, err := ParseEnvelope(env)
		if err != nil {
			t.Fatalf("sealed envelope does not parse: %v", err)
		}
		if n := len(raw) - nonceSize - 16; n%padBlock != 0 {
			t.Errorf("plaintext of %d bytes is not padded to %d", n, padBlock)
		}
		got, err := open(map[string]*Key{k.KID(): k}, env)
		if err != nil || got != m {
			t.Errorf("round trip = %+v, %v; want %+v", got, err, m)
		}
	}
}

func TestSealRefuses(t *testing.T) {
	k := mustKey(t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	for name, m := range map[string]Message{
		"no title":          {Body: "b"},
		"no body":           {Title: "t"},
		"long title":        {Title: strings.Repeat("a", maxTitle+1), Body: "b"},
		"long subtitle":     {Title: "t", Subtitle: strings.Repeat("a", maxTitle+1), Body: "b"},
		"long body":         {Title: "t", Body: strings.Repeat("a", maxBody+1)},
		"javascript url":    {Title: "t", Body: "b", URL: "javascript:alert(1)"},
		"data url":          {Title: "t", Body: "b", URL: "DATA:text/html,x"},
		"http no host":      {Title: "t", Body: "b", URL: "https:///path"},
		"no scheme":         {Title: "t", Body: "b", URL: "example.com/x"},
		"long url":          {Title: "t", Body: "b", URL: "https://example.com/" + strings.Repeat("a", maxURL)},
		"envelope too long": {Title: "Max", Body: strings.Repeat("x", 2242)},
	} {
		if env, err := Seal(k, m, rand.Reader); err == nil {
			t.Errorf("%s: sealed to %s, want an error", name, env)
		}
	}
	// One byte under the limit above is the max_size vector: it seals.
	if _, err := Seal(k, Message{Title: "Max", Body: strings.Repeat("x", 2241)}, rand.Reader); err != nil {
		t.Errorf("largest plaintext: %v", err)
	}
	if _, err := Seal(k, Message{Title: "t", Body: "b"}, bytes.NewReader([]byte{1, 2, 3})); err == nil {
		t.Error("a short nonce read must fail")
	}
}

func TestParseKey(t *testing.T) {
	const want = "767c0806"
	for _, s := range []string{
		"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
		"000102030405060708090A0B0C0D0E0F101112131415161718191A1B1C1D1E1F",
		" 00010203 04050607 08090a0b 0c0d0e0f\n10111213 14151617\t18191a1b 1c1d1e1f\r\n",
	} {
		if k := mustKey(t, s); k.KID() != want {
			t.Errorf("ParseKey(%q).KID() = %s, want %s", s, k.KID(), want)
		}
	}
	for _, s := range []string{
		"",
		"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e",
		"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f00",
		"zz0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
	} {
		if _, err := ParseKey(s); err == nil {
			t.Errorf("ParseKey(%q) accepted", s)
		}
	}
	_, err := ParseKey("hlk_0123456789abcdef0123456789abcdef")
	if err == nil || !strings.Contains(err.Error(), "integration key") {
		t.Errorf("hlk_ key: err = %v, want the integration key hint", err)
	}
}

func TestKeyStringHidesMaterial(t *testing.T) {
	k := mustKey(t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if s := k.String(); s != "e2e key 767c0806" {
		t.Errorf("String() = %q", s)
	}
}
