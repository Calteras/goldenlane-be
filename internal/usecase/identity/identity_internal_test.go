package identity

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProfileOf(t *testing.T) {
	t.Run("a verified email is kept, lower-cased", func(t *testing.T) {
		p := profileOf(Claims{Email: " Rina@Example.com ", EmailVerified: true})
		require.Equal(t, "rina@example.com", p.Email)
	})

	t.Run("an unverified email is dropped", func(t *testing.T) {
		require.Empty(t, profileOf(Claims{Email: "rina@example.com"}).Email)
	})

	t.Run("only an https picture is kept", func(t *testing.T) {
		require.Equal(t, "https://x.test/a.png", profileOf(Claims{Picture: "https://x.test/a.png"}).AvatarURL)
		require.Empty(t, profileOf(Claims{Picture: "http://x.test/a.png"}).AvatarURL)
	})

	t.Run("a long name is cut to 80 characters", func(t *testing.T) {
		require.Len(t, []rune(profileOf(Claims{Name: strings.Repeat("é", 120)}).Name), 80)
	})

	t.Run("a phone sign-in passes the phone through", func(t *testing.T) {
		require.Equal(t, "+62812", profileOf(Claims{Phone: "+62812"}).Phone)
	})
}
