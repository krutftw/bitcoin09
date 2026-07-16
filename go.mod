module github.com/krutftw/bitcoin09

go 1.25.12

require (
	github.com/cosmos/go-bip39 v1.0.0
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
	golang.org/x/crypto v0.53.0
	golang.org/x/sys v0.47.0
)

require (
	golang.org/x/mobile v0.0.0-20260709172247-6129f5bee9d5 // indirect
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/tools v0.48.0 // indirect
)

tool (
	golang.org/x/mobile/cmd/gobind
	golang.org/x/mobile/cmd/gomobile
)
