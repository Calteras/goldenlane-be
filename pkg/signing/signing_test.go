package signing_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/pkg/signing"
)

// Vectors produced by the POS implementation (ansilum-be
// src/domain/streetfood/streetfood-signing.ts). If these fail, the two services
// can no longer talk to each other.
var posVectors = []struct {
	secret, timestamp, method, path, body, signature string
}{
	{
		"sfs_testSecret_abc", "1790000000", "POST",
		"/app/terrapos/integrations/streetfood/orders",
		`{"platformOrderId":"sf_x","items":[{"quantity":2}]}`,
		"d2bc4908777a9c37a2bd92c783c72ea0aee18de17c1dd1006273eef3cae9a216",
	},
	{
		"sfs_testSecret_abc", "1790000001", "get",
		"/app/terrapos/integrations/streetfood/outlets?x=1&y=%C3%A9", "",
		"45a6f0ed5dd5a3d3856d11e562b2ffff835886df41efc70e1d85fd23b90c171b",
	},
	{
		"sfs_ünïcode_秘密", "1790000002", "POST", "/v1/integrations/pos/events",
		`{"events":[{"note":"Rp 5.000 — café ☕"}]}`,
		"670c1d1b9bfb1077c66705efc0f0b5970b91f1ba4e0bb62ff1dc01a6f5490d92",
	},
}

func TestSignMatchesPOS(t *testing.T) {
	for _, v := range posVectors {
		got := signing.Sign(v.secret, v.timestamp, v.method, v.path, []byte(v.body))
		require.Equal(t, v.signature, got, "%s %s", v.method, v.path)
	}
}

func TestVerify(t *testing.T) {
	v := posVectors[0]
	sent, err := strconv.ParseInt(v.timestamp, 10, 64)
	require.NoError(t, err)
	now := time.Unix(sent, 0)
	body := []byte(v.body)

	require.NoError(t, signing.Verify(v.secret, v.timestamp, v.signature, v.method, v.path, body, now))
	require.NoError(t, signing.Verify(v.secret, v.timestamp, v.signature, "post", v.path, body, now), "method is case-insensitive")

	tests := []struct {
		name string
		do   func() error
		want error
	}{
		{"tampered body", func() error {
			return signing.Verify(v.secret, v.timestamp, v.signature, v.method, v.path, []byte(v.body+" "), now)
		}, signing.ErrSignatureInvalid},
		{"other path", func() error {
			return signing.Verify(v.secret, v.timestamp, v.signature, v.method, v.path+"/x", body, now)
		}, signing.ErrSignatureInvalid},
		{"wrong secret", func() error {
			return signing.Verify("other", v.timestamp, v.signature, v.method, v.path, body, now)
		}, signing.ErrSignatureInvalid},
		{"garbage signature", func() error {
			return signing.Verify(v.secret, v.timestamp, "zz", v.method, v.path, body, now)
		}, signing.ErrSignatureInvalid},
		{"garbage timestamp", func() error {
			return signing.Verify(v.secret, "abc", v.signature, v.method, v.path, body, now)
		}, signing.ErrSignatureInvalid},
		{"too old", func() error {
			return signing.Verify(v.secret, v.timestamp, v.signature, v.method, v.path, body, now.Add(6*time.Minute))
		}, signing.ErrTimestampSkew},
		{"too new", func() error {
			return signing.Verify(v.secret, v.timestamp, v.signature, v.method, v.path, body, now.Add(-6*time.Minute))
		}, signing.ErrTimestampSkew},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { require.ErrorIs(t, tc.do(), tc.want) })
	}

	require.NoError(t, signing.Verify(v.secret, v.timestamp, v.signature, v.method, v.path, body, now.Add(299*time.Second)))
}
