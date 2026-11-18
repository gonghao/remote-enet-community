package main

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"
)

func TestMultiResponders(t *testing.T) {
	got, err := decodeMultiResponders([]byte(`{"22F186":[{"source":16,"payload":"62f18601"},{"source":99,"payload":"62f18602"}],"22f190":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got["22f186"]) != 2 || got["22f186"][0].Src != 16 || got["22f186"][1].Src != 99 || !bytes.Equal(got["22f186"][0].Payload, []byte{0x62, 0xf1, 0x86, 1}) {
		t.Fatalf("explicit source/payload lost: %+v", got)
	}
	if values, exists := got["22f190"]; !exists || values == nil || len(values) != 0 {
		t.Fatalf("empty override lost: %+v", got)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := run(ctx, []string{"--multi-responders", filepath.Join("fixtures", "m0b-readonly.json"), "--diag-port", "0", "--ctrl-port", "0", "--disc-port", "0"}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidMultiResponders(t *testing.T) {
	for _, raw := range []string{
		`{"22f186":[{"payload":"62f18601"}]}`,
		`{"22f186":[{"source":16}]}`,
		`{"22f186":[{"source":256,"payload":"62f18601"}]}`,
		`{"22f186":[{"source":16,"payload":"zz"}]}`,
		`{"22f186":[{"source":16,"payload":"62f18601","typo":true}]}`,
		`{"xx":[]}`,
		`{"":[]}`,
		`{"22F186":[],"22f186":[]}`,
		`{} {}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := decodeMultiResponders([]byte(raw)); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
