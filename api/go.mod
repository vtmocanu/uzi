module github.com/vtmocanu/uzi/api

go 1.26.4

// Build with the patched 1.26.x that fixes the stdlib CVEs govulncheck gates on
// (net/http, crypto/tls, net/url, encoding/asn1, ...). Go auto-downloads it; the
// renovate operator keeps this current. `go 1.26.4` above stays the language floor.
toolchain go1.26.6

require (
	charm.land/bubbletea/v2 v2.0.8
	charm.land/glamour/v2 v2.0.1
	charm.land/lipgloss/v2 v2.0.6
	code.gitea.io/sdk/gitea v0.25.1
	github.com/BurntSushi/toml v1.6.0
	github.com/charmbracelet/x/ansi v0.11.8
	github.com/coder/websocket v1.8.15
	github.com/coreos/go-oidc/v3 v3.14.1
	github.com/go-chi/chi/v5 v5.3.1
	github.com/go-git/go-git/v5 v5.19.2
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/google/go-github/v90 v90.0.0
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.10.0
	github.com/pressly/goose/v3 v3.27.3
	github.com/robfig/cron/v3 v3.0.1
	github.com/slack-go/slack v0.27.0
	github.com/spf13/cobra v1.10.2
	github.com/yuin/goldmark v1.7.17
	gitlab.com/gitlab-org/api/client-go/v2 v2.44.0
	golang.org/x/crypto v0.54.0
	golang.org/x/mod v0.39.0
	golang.org/x/oauth2 v0.36.0
	golang.org/x/sync v0.22.0
)

require (
	dario.cat/mergo v1.0.0 // indirect
	github.com/42wim/httpsig v1.2.4 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/ProtonMail/go-crypto v1.1.6 // indirect
	github.com/alecthomas/chroma/v2 v2.14.0 // indirect
	github.com/aymerick/douceur v0.2.0 // indirect
	github.com/charmbracelet/colorprofile v0.4.3 // indirect
	github.com/charmbracelet/ultraviolet v0.0.0-20260811164956-006e29f97886 // indirect
	github.com/charmbracelet/x/exp/slice v0.0.0-20250327172914-2fdc97757edf // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/cyphar/filepath-securejoin v0.6.1 // indirect
	github.com/davidmz/go-pageant v1.0.2 // indirect
	github.com/dlclark/regexp2 v1.11.0 // indirect
	github.com/emirpasic/gods v1.18.1 // indirect
	github.com/go-fed/httpsig v1.1.0 // indirect
	github.com/go-git/gcfg v1.5.1-0.20230307220236-3a3c6141e376 // indirect
	github.com/go-git/go-billy/v5 v5.9.0 // indirect
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/google/go-querystring v1.2.0 // indirect
	github.com/gorilla/css v1.0.1 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/hashicorp/go-retryablehttp v0.7.8 // indirect
	github.com/hashicorp/go-version v1.9.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jbenet/go-context v0.0.0-20150711004518-d14ea06fba99 // indirect
	github.com/kevinburke/ssh_config v1.2.0 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mattn/go-runewidth v0.0.24 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/microcosm-cc/bluemonday v1.0.27 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/pjbgf/sha1cd v0.6.0 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/sergi/go-diff v1.3.2-0.20230802210424-5b0b94c5c0d3 // indirect
	github.com/sethvargo/go-retry v0.4.0 // indirect
	github.com/skeema/knownhosts v1.3.1 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	github.com/xanzy/ssh-agent v0.3.3 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	github.com/yuin/goldmark-emoji v1.0.5 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	gopkg.in/warnings.v0 v0.1.2 // indirect
)
