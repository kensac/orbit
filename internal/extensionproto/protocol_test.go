package extensionproto

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

const request = `{"protocol":"orbit.extension.v1","invocation_id":"inv_1","operation":"reduce","parameters":{"amount":9007199254740993.01},"inputs":[]}`
const response = `{"protocol":"orbit.extension.v1","invocation_id":"inv_1","status":"ok","result":{"amount":9007199254740993.01}}`

func TestDeterministicExactDecimalRoundTrip(t *testing.T) {
	r, err := DecodeRequest(strings.NewReader(request), "reduce", nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := EncodeRequest(*r, "reduce", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EncodeRequest(*r, "reduce", nil)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("unstable encoding: %s / %s (%v)", first, second, err)
	}
	if !bytes.Contains(first, []byte("9007199254740993.01")) {
		t.Fatal("decimal rounded")
	}
	out, err := DecodeResponse(strings.NewReader(response), "inv_1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncodeResponse(*out, "inv_1")
	if err != nil || !bytes.Contains(b, []byte("9007199254740993.01")) {
		t.Fatalf("result decimal: %s %v", b, err)
	}
}

func TestRejectRequestEnvelopes(t *testing.T) {
	cases := map[string]string{
		"duplicate":        strings.Replace(request, `"operation":"reduce"`, `"operation":"reduce","operation":"reduce"`, 1),
		"nested duplicate": strings.Replace(request, `"amount":9007199254740993.01`, `"x":1,"x":2`, 1),
		"case alias":       strings.Replace(request, `"operation"`, `"Operation"`, 1),
		"unknown":          strings.Replace(request, `"inputs":[]`, `"inputs":[],"command":"x"`, 1),
		"null":             strings.Replace(request, `"inputs":[]`, `"inputs":null`, 1),
		"missing":          strings.Replace(request, `,"inputs":[]`, "", 1),
		"wrong protocol":   strings.Replace(request, "orbit.extension.v1", "orbit.extension.v2", 1),
		"wrong operation":  strings.Replace(request, "reduce", "other", 1),
		"unsafe id":        strings.Replace(request, "inv_1", "../secret", 1),
		"trailing":         request + " {}",
		"trailing byte":    request + "x",
		"truncated":        request[:len(request)-1],
		"invalid UTF8":     request + string([]byte{255}),
		"exponent":         strings.Replace(request, "9007199254740993.01", "1e309", 1),
		"long number":      strings.Replace(request, "9007199254740993.01", strings.Repeat("1", 129), 1),
		"deep":             strings.Replace(request, `{"amount":9007199254740993.01}`, strings.Repeat("[", 65)+"0"+strings.Repeat("]", 65), 1),
		"oversized":        request + strings.Repeat(" ", MaxJSONBytes),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeRequest(strings.NewReader(b), "reduce", nil); err == nil {
				t.Fatal("accepted invalid envelope")
			}
		})
	}
}

func TestResponseStatesAndCorrelation(t *testing.T) {
	for _, good := range []string{response, strings.Replace(response, `{"amount":9007199254740993.01}`, "null", 1), `{"protocol":"orbit.extension.v1","invocation_id":"inv_1","status":"error","error":{"code":"bad-input","message":"invalid data"}}`} {
		if _, err := DecodeResponse(strings.NewReader(good), "inv_1"); err != nil {
			t.Fatal(err)
		}
	}
	for name, b := range map[string]string{
		"wrong id":            strings.Replace(response, "inv_1", "inv_2", 1),
		"both":                strings.Replace(response, `"result":`, `"error":{"code":"failed","message":"failed"},"result":`, 1),
		"missing result":      strings.Replace(response, `,"result":{"amount":9007199254740993.01}`, "", 1),
		"null error":          strings.Replace(response, `"status":"ok","result":{"amount":9007199254740993.01}`, `"status":"error","error":null`, 1),
		"unknown error field": `{"protocol":"orbit.extension.v1","invocation_id":"inv_1","status":"error","error":{"code":"failed","message":"failed","url":"x"}}`,
		"message bound":       `{"protocol":"orbit.extension.v1","invocation_id":"inv_1","status":"error","error":{"code":"failed","message":"` + strings.Repeat("x", 4097) + `"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeResponse(strings.NewReader(b), "inv_1"); err == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
}

func TestFileDeclarations(t *testing.T) {
	f := File{Name: "data", Path: "inputs/data.json", MediaType: "application/json", Bytes: 3, SHA256: strings.Repeat("a", 64)}
	makeRequest := func(fs []File) string {
		r := Request{Protocol: Protocol, InvocationID: "inv_1", Operation: "reduce", Parameters: json.RawMessage(`{}`), Inputs: fs}
		b, _ := json.Marshal(r)
		return string(b)
	}
	if _, err := DecodeRequest(strings.NewReader(makeRequest([]File{f})), "reduce", []string{"data"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".", "../x", "/work/out/x", "a/../x", "a//b", "a\\b", "a\x00b"} {
		bad := f
		bad.Path = p
		if _, err := DecodeRequest(strings.NewReader(makeRequest([]File{bad})), "reduce", []string{"data"}); err == nil {
			t.Errorf("accepted path %q", p)
		}
	}
	for name, mutate := range map[string]func(*File){"negative": func(f *File) { f.Bytes = -1 }, "large": func(f *File) { f.Bytes = MaxFileBytes + 1 }, "hash": func(f *File) { f.SHA256 = strings.Repeat("A", 64) }, "media": func(f *File) { f.MediaType = "text/plain\r\nx" }} {
		t.Run(name, func(t *testing.T) {
			bad := f
			mutate(&bad)
			if _, err := DecodeRequest(strings.NewReader(makeRequest([]File{bad})), "reduce", []string{"data"}); err == nil {
				t.Fatal("accepted invalid descriptor")
			}
		})
	}
	for name, b := range map[string]string{"unknown": strings.Replace(makeRequest([]File{f}), `"name":"data"`, `"name":"data","url":"x"`, 1), "missing": strings.Replace(makeRequest([]File{f}), `"bytes":3,`, "", 1), "null": strings.Replace(makeRequest([]File{f}), `"bytes":3`, `"bytes":null`, 1), "duplicate": makeRequest([]File{f, f})} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeRequest(strings.NewReader(b), "reduce", []string{"data"}); err == nil {
				t.Fatal("accepted invalid files")
			}
		})
	}
	if _, err := DecodeRequest(strings.NewReader(makeRequest([]File{f})), "reduce", nil); err == nil {
		t.Fatal("accepted undeclared slot")
	}
}

type countingReader struct{ n int }

func (r *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	r.n += len(p)
	return len(p), nil
}
func TestReadBound(t *testing.T) {
	r := &countingReader{}
	if _, err := DecodeResponse(r, "inv_1"); err == nil {
		t.Fatal("accepted whitespace")
	}
	if r.n != MaxJSONBytes+1 {
		t.Fatalf("read %d bytes", r.n)
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestReaderError(t *testing.T) {
	if _, err := DecodeRequest(brokenReader{}, "reduce", nil); err == nil {
		t.Fatal("lost read error")
	}
}

func TestAdditionalAdversarialBounds(t *testing.T) {
	escaped := strings.Replace(request, `"operation":"reduce"`, `"operation":"reduce","oper\u0061tion":"reduce"`, 1)
	if _, err := DecodeRequest(strings.NewReader(escaped), "reduce", nil); err == nil {
		t.Fatal("accepted escaped duplicate key")
	}
	f := File{Name: "data", Path: "data", MediaType: "text/plain", Bytes: MaxFileBytes, SHA256: strings.Repeat("a", 64)}
	g := f
	g.Name = "other"
	g.Path = "other"
	g.Bytes = 1
	for name, fs := range map[string][]File{"total": {f, g}, "count": make([]File, MaxFiles+1)} {
		t.Run(name, func(t *testing.T) {
			r := Response{Protocol: Protocol, InvocationID: "inv_1", Status: "ok", Result: json.RawMessage(`0`), Outputs: fs}
			if _, err := EncodeResponse(r, "inv_1"); err == nil {
				t.Fatal("accepted oversized output declarations")
			}
		})
	}
	r := Request{Protocol: Protocol, InvocationID: "inv_1", Operation: "reduce", Parameters: json.RawMessage(strings.Repeat(" ", MaxJSONBytes+1)), Inputs: []File{}}
	if _, err := EncodeRequest(r, "reduce", nil); err == nil {
		t.Fatal("accepted oversized parameters")
	}
	out := Response{Protocol: Protocol, InvocationID: "inv_1", Status: "ok", Result: json.RawMessage(strings.Repeat(" ", MaxJSONBytes+1))}
	if _, err := EncodeResponse(out, "inv_1"); err == nil {
		t.Fatal("accepted oversized result")
	}
	b, _ := json.Marshal(Request{Protocol: Protocol, InvocationID: "inv_1", Operation: "reduce", Parameters: json.RawMessage(`{}`), Inputs: []File{f}})
	b = bytes.Replace(b, []byte(`"name":"data"`), []byte(`"Name":"data"`), 1)
	if _, err := DecodeRequest(bytes.NewReader(b), "reduce", []string{"data"}); err == nil {
		t.Fatal("accepted descriptor case alias")
	}
}
