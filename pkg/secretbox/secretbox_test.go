package secretbox_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/pkg/secretbox"
)

func TestSealOpen(t *testing.T) {
	box, err := secretbox.New(bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)

	sealed, err := box.Seal("sfs_secret", "merchant-1")
	require.NoError(t, err)
	require.NotContains(t, sealed, "sfs_secret")

	plain, err := box.Open(sealed, "merchant-1")
	require.NoError(t, err)
	require.Equal(t, "sfs_secret", plain)

	_, err = box.Open(sealed, "merchant-2")
	require.ErrorIs(t, err, secretbox.ErrInvalid, "bound to its context")

	again, _ := box.Seal("sfs_secret", "merchant-1")
	require.NotEqual(t, sealed, again, "fresh nonce every time")

	_, err = box.Open("garbage", "merchant-1")
	require.ErrorIs(t, err, secretbox.ErrInvalid)

	_, err = secretbox.New([]byte("short"))
	require.Error(t, err)
}
