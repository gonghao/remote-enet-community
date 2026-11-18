package hsfz

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"testing/iotest"
)

func TestIdentification(t *testing.T) {
	raw, _ := hex.DecodeString("000000320011444941474144523130424d574d4143414142424343444445454646424d5756494e5742415858585858585830303030303031")
	if len(raw) != 56 {
		t.Fatal(len(raw))
	}
	f, err := ReadFrame(bytes.NewReader(raw), 50)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ParseIdentification(f.Body)
	if err != nil {
		t.Fatal(err)
	}
	if id.DiagAddr != 0x10 || id.VIN != "WBAXXXXXXX0000001" || id.MAC != [6]byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff} {
		t.Fatalf("%+v", id)
	}
	if !bytes.Equal(id.MarshalBody(), raw[6:]) {
		t.Fatal("round trip")
	}
	extended := append([]byte("UNKNOWNvalue"), append(f.Body, []byte("EXTRAdata")...)...)
	id, err = ParseIdentification(extended)
	if err != nil {
		t.Fatal(err)
	}
	id.DiagAddr = 0x42
	id.VIN = "12345678901234567"
	b := id.MarshalBody()
	if !bytes.HasPrefix(b, []byte("UNKNOWNvalueDIAGADR42")) || !bytes.HasSuffix(b, []byte("EXTRAdata")) {
		t.Fatalf("%q", b)
	}
	for _, b := range [][]byte{nil, []byte("DIAGADRZZBMWMACAABBCCDDEEFFBMWVIN12345678901234567"), []byte("DIAGADR10BMWMACZZBBCCDDEEFFBMWVIN12345678901234567"), []byte("DIAGADR10BMWMACAABBCCDDEEFFBMWVINshort")} {
		if _, err := ParseIdentification(b); err == nil {
			t.Fatalf("accepted %q", b)
		}
	}
}
func TestFraming(t *testing.T) {
	frames := []Frame{{ControlAliveCheck, bytes.Repeat([]byte("a"), 32)}, {ControlAliveCheck, []byte{0, 0xf4}}, {ControlErrIncorrectControlWord, nil}, AliveResponse(0xf5), {Control(0xabcd), nil}}
	var b bytes.Buffer
	for _, f := range frames {
		if err := WriteFrame(&b, f); err != nil {
			t.Fatal(err)
		}
	}
	for _, wrap := range []func(io.Reader) io.Reader{iotest.OneByteReader, iotest.HalfReader, iotest.DataErrReader} {
		r := wrap(bytes.NewReader(b.Bytes()))
		for _, want := range frames {
			got, err := ReadFrame(r, 32)
			if err != nil || got.Control != want.Control || !bytes.Equal(got.Body, want.Body) {
				t.Fatalf("%+v %v", got, err)
			}
		}
		if _, err := ReadFrame(r, 32); !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	}
	for _, raw := range [][]byte{{0}, {0, 0, 0, 2, 0, 1, 1}} {
		if _, err := ReadFrame(bytes.NewReader(raw), 32); err == nil {
			t.Fatal("accepted truncation")
		}
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff, 0, 1}), 32); err == nil {
		t.Fatal("oversize")
	}
}
func TestAck(t *testing.T) {
	raw, _ := hex.DecodeString("000000050001f46322f101")
	f, _ := ReadFrame(bytes.NewReader(raw), 10)
	d, _ := ParseDiagBody(f.Body)
	got, _ := AckFor(d).MarshalBinary()
	raw[5] = 2
	if !bytes.Equal(got, raw) {
		t.Fatalf("%x", got)
	}
	for n := 0; n < 12; n++ {
		d.Payload = bytes.Repeat([]byte{1}, n)
		a := AckFor(d)
		if len(a.Body) != 2+min(n, 5) || a.Body[0] != 0xf4 || a.Body[1] != 0x63 {
			t.Fatal(a)
		}
	}
	if _, err := ParseDiagBody([]byte{1}); err == nil {
		t.Fatal("short body")
	}
}

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(p []byte) (int, error) { return w.Buffer.Write(p[:min(2, len(p))]) }

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }
func TestShortWrites(t *testing.T) {
	w := new(shortWriter)
	if err := WriteFrame(w, AliveResponse(0xf4)); err != nil {
		t.Fatal(err)
	}
	if w.Len() != 8 {
		t.Fatal(w.Len())
	}
	if err := WriteFrame(zeroWriter{}, Frame{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
}
