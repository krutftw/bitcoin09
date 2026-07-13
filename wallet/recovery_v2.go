package wallet

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"

	bip39 "github.com/cosmos/go-bip39"
	"github.com/krutftw/bitcoin09/core"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/pbkdf2"
)

const (
	SchemaVersionV2 = 2

	RecoveryEntropyBytes = 32
	RecoveryWordCount    = 24
	RecoveryDerivationID = "slip10-ed25519:m/9009'/network'/0'/index'"

	v2KDFName    = "argon2id"
	v2CipherName = "xchacha20poly1305"
	v2SaltBytes  = 16
	v2NonceBytes = chacha20poly1305.NonceSizeX

	minV2KDFMemoryKiB = 8 * 1024
	maxV2KDFMemoryKiB = 256 * 1024
	minV2KDFTime      = 1
	maxV2KDFTime      = 10
	maxV2KDFParallel  = 16
)

var (
	ErrWalletUnlock   = errors.New("wallet could not be unlocked")
	ErrWalletV2Format = errors.New("wallet V2 format is invalid")

	walletV2Random      io.Reader = rand.Reader
	walletV2KDFParams             = defaultV2KDFParams(uint8(min(runtime.NumCPU(), 255)))
	recoveryWordList              = append([]string(nil), bip39.EnglishWordList...)
	recoveryWordIndexes           = buildRecoveryWordIndexes(recoveryWordList)
)

func buildRecoveryWordIndexes(words []string) map[string]uint16 {
	indexes := make(map[string]uint16, len(words))
	for index, word := range words {
		indexes[word] = uint16(index)
	}
	return indexes
}

type v2KDFParams struct {
	Name        string `json:"name"`
	Salt        string `json:"salt"`
	Time        uint32 `json:"time"`
	MemoryKiB   uint32 `json:"memory_kib"`
	Parallelism uint8  `json:"parallelism"`
}

type v2Cipher struct {
	Name       string `json:"name"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

type v2Envelope struct {
	SchemaVersion int         `json:"schema_version"`
	Network       string      `json:"network"`
	KDF           v2KDFParams `json:"kdf"`
	Cipher        v2Cipher    `json:"cipher"`
}

type recoveryPayload struct {
	Entropy      []byte `json:"entropy"`
	AddressCount uint32 `json:"address_count"`
	Derivation   string `json:"derivation"`
}

type v2AuthenticatedHeader struct {
	SchemaVersion int         `json:"schema_version"`
	Network       string      `json:"network"`
	KDF           v2KDFParams `json:"kdf"`
	CipherName    string      `json:"cipher_name"`
}

// CreateV2 creates a new encrypted deterministic wallet. The returned phrase
// is the only phrase that can reconstruct its address sequence and is not
// stored in the cleartext wallet envelope.
func CreateV2(path, network string, password []byte) (w *Wallet, phrase string, err error) {
	entropy := make([]byte, RecoveryEntropyBytes)
	if _, err := io.ReadFull(walletV2Random, entropy); err != nil {
		clear(entropy)
		return nil, "", fmt.Errorf("read wallet recovery entropy: %w", err)
	}
	defer clear(entropy)
	phrase, err = recoveryPhraseFromEntropy(entropy)
	if err != nil {
		return nil, "", err
	}
	w, err = createV2FromEntropy(path, network, password, entropy, 1)
	if err != nil {
		return nil, "", err
	}
	return w, phrase, nil
}

// RestoreV2 creates a separate encrypted wallet from an existing recovery
// phrase. addressCount is explicit until the wallet's bounded history scanner
// has completed; callers must never imply that legacy random keys are covered.
func RestoreV2(path, network string, password []byte, phrase string, addressCount uint32) (*Wallet, error) {
	entropy, err := recoveryEntropyFromPhrase(phrase)
	if err != nil {
		return nil, err
	}
	defer clear(entropy)
	return createV2FromEntropy(path, network, password, entropy, addressCount)
}

// OpenV2 unlocks an existing Wallet V2 file. It rejects V1 and legacy files
// without rewriting them.
func OpenV2(path, network string, password []byte) (*Wallet, error) {
	if network != core.MainNetMachineID && network != core.RegTestMachineID {
		return nil, fmt.Errorf("unsupported wallet network %q", network)
	}
	canonicalPath, err := canonicalWalletPath(path)
	if err != nil {
		return nil, err
	}
	w := &Wallet{
		path:       canonicalPath,
		network:    network,
		requireV2:  true,
		v2Password: append([]byte(nil), password...),
	}
	if err := w.withKeys(true, func(_ []ed25519.PrivateKey) error { return nil }); err != nil {
		w.Close()
		return nil, err
	}
	return w, nil
}

// RecoveryPhrase decrypts and returns the canonical 24-word phrase for an
// unlocked Wallet V2 handle. Callers should display it only after an explicit
// local re-authentication step and must not log or persist the returned value.
func (w *Wallet) RecoveryPhrase() (phrase string, err error) {
	if w == nil || !w.requireV2 || len(w.v2Password) == 0 {
		return "", ErrWalletUnlock
	}
	lock, err := acquireWalletFileLock(w.path + ".lock")
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, lock.release()) }()
	if err := rejectWalletHardLink(w.path); err != nil {
		return "", err
	}
	payload, err := w.readRecoveryPayloadLocked()
	if err != nil {
		return "", err
	}
	defer clear(payload.Entropy)
	return recoveryPhraseFromEntropy(payload.Entropy)
}

func createV2FromEntropy(path, network string, password, entropy []byte, addressCount uint32) (w *Wallet, err error) {
	if network != core.MainNetMachineID && network != core.RegTestMachineID {
		return nil, fmt.Errorf("unsupported wallet network %q", network)
	}
	canonicalPath, err := canonicalWalletPath(path)
	if err != nil {
		return nil, err
	}
	w = &Wallet{
		path:       canonicalPath,
		network:    network,
		requireV2:  true,
		v2Password: append([]byte(nil), password...),
	}
	defer func() {
		if err != nil {
			w.Close()
		}
	}()
	lock, err := acquireWalletFileLock(w.path + ".lock")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, lock.release()) }()
	if err := rejectWalletHardLink(w.path); err != nil {
		return nil, err
	}
	existing, state, err := w.readWalletBytesLocked()
	clear(existing)
	if err != nil {
		return nil, err
	}
	if state != walletMissing {
		return nil, errors.New("wallet file already exists")
	}
	payload := recoveryPayload{
		Entropy:      append([]byte(nil), entropy...),
		AddressCount: addressCount,
		Derivation:   RecoveryDerivationID,
	}
	defer clear(payload.Entropy)
	if err := validateRecoveryPayload(payload); err != nil {
		return nil, err
	}
	if err := w.writeRecoveryPayloadLocked(payload); err != nil {
		return nil, err
	}
	keys, state, err := w.readKeysLocked()
	if err != nil {
		return nil, err
	}
	wipeKeys(keys)
	if state != walletV2 {
		return nil, walletV2FormatError("created wallet did not reopen as V2")
	}
	return w, nil
}

func defaultV2KDFParams(parallelism uint8) v2KDFParams {
	if parallelism < 1 {
		parallelism = 1
	}
	if parallelism > 4 {
		parallelism = 4
	}
	return v2KDFParams{
		Name:        v2KDFName,
		Time:        3,
		MemoryKiB:   64 * 1024,
		Parallelism: parallelism,
	}
}

func v2TestKDFParams() v2KDFParams {
	return v2KDFParams{
		Name:        v2KDFName,
		Time:        minV2KDFTime,
		MemoryKiB:   minV2KDFMemoryKiB,
		Parallelism: 1,
	}
}

func recoveryPhraseFromEntropy(entropy []byte) (string, error) {
	mnemonic, err := recoveryMnemonicBytesFromEntropy(entropy)
	if err != nil {
		return "", err
	}
	defer clear(mnemonic)
	phrase := string(mnemonic)
	if len(strings.Fields(phrase)) != RecoveryWordCount {
		return "", errors.New("recovery phrase word count mismatch")
	}
	return phrase, nil
}

// recoveryMnemonicBytesFromEntropy keeps the canonical BIP39 sentence in a
// wipeable buffer. It is used internally so opening a wallet never needs to
// materialize the recovery phrase as an immutable Go string.
func recoveryMnemonicBytesFromEntropy(entropy []byte) ([]byte, error) {
	if len(entropy) != RecoveryEntropyBytes {
		return nil, fmt.Errorf("recovery entropy must be %d bytes", RecoveryEntropyBytes)
	}
	checksum := sha256.Sum256(entropy)
	mnemonic := make([]byte, 0, 24*9)
	for word := 0; word < RecoveryWordCount; word++ {
		var index uint16
		for offset := 0; offset < 11; offset++ {
			position := word*11 + offset
			var bit byte
			if position < RecoveryEntropyBytes*8 {
				bit = (entropy[position/8] >> (7 - (position % 8))) & 1
			} else {
				checksumPosition := position - RecoveryEntropyBytes*8
				bit = (checksum[checksumPosition/8] >> (7 - (checksumPosition % 8))) & 1
			}
			index = index<<1 | uint16(bit)
		}
		if word > 0 {
			mnemonic = append(mnemonic, ' ')
		}
		mnemonic = append(mnemonic, recoveryWordList[index]...)
	}
	clear(checksum[:])
	return mnemonic, nil
}

func recoverySeedFromEntropy(entropy []byte) ([]byte, error) {
	mnemonic, err := recoveryMnemonicBytesFromEntropy(entropy)
	if err != nil {
		return nil, err
	}
	defer clear(mnemonic)
	return pbkdf2.Key(mnemonic, []byte("mnemonic"), 2048, 64, sha512.New), nil
}

func recoveryEntropyFromPhrase(phrase string) ([]byte, error) {
	words := strings.Fields(phrase)
	canonical := strings.Join(words, " ")
	if phrase != canonical || len(words) != RecoveryWordCount {
		return nil, errors.New("recovery phrase must contain exactly 24 canonical words")
	}
	withChecksum := make([]byte, RecoveryEntropyBytes+1)
	defer clear(withChecksum)
	for wordPosition, word := range words {
		index, ok := recoveryWordIndexes[word]
		if !ok {
			return nil, errors.New("recovery phrase checksum or words are invalid")
		}
		for bitPosition := 0; bitPosition < 11; bitPosition++ {
			if index&(1<<uint(10-bitPosition)) == 0 {
				continue
			}
			position := wordPosition*11 + bitPosition
			withChecksum[position/8] |= 1 << uint(7-(position%8))
		}
	}
	entropy := append([]byte(nil), withChecksum[:RecoveryEntropyBytes]...)
	checksum := sha256.Sum256(entropy)
	valid := withChecksum[RecoveryEntropyBytes] == checksum[0]
	clear(checksum[:])
	if !valid {
		clear(entropy)
		return nil, errors.New("recovery phrase checksum or words are invalid")
	}
	return entropy, nil
}

// deriveSLIP10Ed25519 treats each path component as a hardened child index.
// Callers pass the human index without the 2^31 hardened offset.
func deriveSLIP10Ed25519(seed []byte, path []uint32) (private, chainCode [32]byte, err error) {
	if len(seed) < 16 || len(seed) > 64 {
		return private, chainCode, errors.New("SLIP-0010 seed length out of range")
	}
	mac := hmac.New(sha512.New, []byte("ed25519 seed"))
	_, _ = mac.Write(seed)
	sum := mac.Sum(nil)
	copy(private[:], sum[:32])
	copy(chainCode[:], sum[32:])
	clear(sum)

	for _, component := range path {
		if component >= 1<<31 {
			clear(private[:])
			clear(chainCode[:])
			return private, chainCode, errors.New("SLIP-0010 path component out of range")
		}
		var data [1 + 32 + 4]byte
		copy(data[1:33], private[:])
		binary.BigEndian.PutUint32(data[33:], component|(1<<31))
		mac = hmac.New(sha512.New, chainCode[:])
		_, _ = mac.Write(data[:])
		sum = mac.Sum(nil)
		copy(private[:], sum[:32])
		copy(chainCode[:], sum[32:])
		clear(data[:])
		clear(sum)
	}
	return private, chainCode, nil
}

func deriveRecoveryPrivateKey(phrase, network string, index uint32) (ed25519.PrivateKey, error) {
	if index >= 1<<31 {
		return nil, errors.New("recovery address index out of range")
	}
	entropy, err := recoveryEntropyFromPhrase(phrase)
	if err != nil {
		return nil, err
	}
	defer clear(entropy)
	seed, err := recoverySeedFromEntropy(entropy)
	if err != nil {
		return nil, err
	}
	defer clear(seed)
	return deriveRecoveryPrivateKeyFromSeed(seed, network, index)
}

func deriveRecoveryPrivateKeyFromEntropy(entropy []byte, network string, index uint32) (ed25519.PrivateKey, error) {
	seed, err := recoverySeedFromEntropy(entropy)
	if err != nil {
		return nil, err
	}
	defer clear(seed)
	return deriveRecoveryPrivateKeyFromSeed(seed, network, index)
}

func deriveRecoveryPrivateKeyFromSeed(seed []byte, network string, index uint32) (ed25519.PrivateKey, error) {
	if index >= 1<<31 {
		return nil, errors.New("recovery address index out of range")
	}
	var networkBranch uint32
	switch network {
	case core.MainNetMachineID:
		networkBranch = 0
	case core.RegTestMachineID:
		networkBranch = 1
	default:
		return nil, fmt.Errorf("unsupported wallet network %q", network)
	}
	privateSeed, chainCode, err := deriveSLIP10Ed25519(seed, []uint32{9009, networkBranch, 0, index})
	if err != nil {
		return nil, err
	}
	defer clear(privateSeed[:])
	defer clear(chainCode[:])
	return ed25519.NewKeyFromSeed(privateSeed[:]), nil
}

func recoveryKeysFromPayload(payload recoveryPayload, network string) ([]ed25519.PrivateKey, error) {
	if err := validateRecoveryPayload(payload); err != nil {
		return nil, err
	}
	seed, err := recoverySeedFromEntropy(payload.Entropy)
	if err != nil {
		return nil, err
	}
	defer clear(seed)
	keys := make([]ed25519.PrivateKey, 0, payload.AddressCount)
	for index := uint32(0); index < payload.AddressCount; index++ {
		key, err := deriveRecoveryPrivateKeyFromSeed(seed, network, index)
		if err != nil {
			wipeKeys(keys)
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func (w *Wallet) readRecoveryPayloadLocked() (recoveryPayload, error) {
	var payload recoveryPayload
	if w == nil || !w.requireV2 || len(w.v2Password) == 0 {
		return payload, ErrWalletUnlock
	}
	encoded, state, err := w.readWalletBytesLocked()
	if err != nil {
		return payload, err
	}
	if state == walletMissing {
		return payload, ErrWalletNotFound
	}
	defer clear(encoded)
	return openRecoveryPayload(encoded, w.v2Password, w.network)
}

func (w *Wallet) writeRecoveryPayloadLocked(payload recoveryPayload) error {
	if w == nil || !w.requireV2 || len(w.v2Password) == 0 {
		return ErrWalletUnlock
	}
	if err := rejectWalletHardLink(w.path); err != nil {
		return err
	}
	encoded, err := sealRecoveryPayload(
		w.network,
		w.v2Password,
		payload,
		walletV2Random,
		walletV2KDFParams,
	)
	if err != nil {
		return err
	}
	defer clear(encoded)
	return durableReplaceWalletFile(w.path, encoded, 0600)
}

func sealRecoveryPayload(network string, password []byte, payload recoveryPayload, randomness io.Reader, params v2KDFParams) ([]byte, error) {
	if network != core.MainNetMachineID && network != core.RegTestMachineID {
		return nil, fmt.Errorf("unsupported wallet network %q", network)
	}
	if len(password) < 1 || len(password) > 1024 {
		return nil, errors.New("wallet password length out of range")
	}
	if err := validateRecoveryPayload(payload); err != nil {
		return nil, err
	}
	if err := validateV2KDFParams(params, false); err != nil {
		return nil, err
	}
	if randomness == nil {
		return nil, errors.New("nil wallet randomness source")
	}

	salt := make([]byte, v2SaltBytes)
	if _, err := io.ReadFull(randomness, salt); err != nil {
		return nil, fmt.Errorf("read wallet salt: %w", err)
	}
	nonce := make([]byte, v2NonceBytes)
	if _, err := io.ReadFull(randomness, nonce); err != nil {
		clear(salt)
		return nil, fmt.Errorf("read wallet nonce: %w", err)
	}
	params.Name = v2KDFName
	params.Salt = base64.RawStdEncoding.EncodeToString(salt)
	envelope := v2Envelope{
		SchemaVersion: SchemaVersionV2,
		Network:       network,
		KDF:           params,
		Cipher: v2Cipher{
			Name:  v2CipherName,
			Nonce: base64.RawStdEncoding.EncodeToString(nonce),
		},
	}
	associatedData, err := v2AssociatedData(envelope)
	if err != nil {
		clear(salt)
		clear(nonce)
		return nil, err
	}
	plaintext, err := json.Marshal(payload)
	if err != nil {
		clear(salt)
		clear(nonce)
		return nil, err
	}
	key := argon2.IDKey(password, salt, params.Time, params.MemoryKiB, params.Parallelism, chacha20poly1305.KeySize)
	clear(salt)
	aead, err := chacha20poly1305.NewX(key)
	clear(key)
	if err != nil {
		clear(nonce)
		clear(plaintext)
		return nil, err
	}
	ciphertext := aead.Seal(nil, nonce, plaintext, associatedData)
	clear(nonce)
	clear(plaintext)
	envelope.Cipher.Ciphertext = base64.RawStdEncoding.EncodeToString(ciphertext)
	clear(ciphertext)
	encoded, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')
	if len(encoded) > MaxWalletFileBytes {
		clear(encoded)
		return nil, errors.New("wallet V2 encoding exceeds size bound")
	}
	return encoded, nil
}

func openRecoveryPayload(encoded, password []byte, expectedNetwork string) (recoveryPayload, error) {
	var payload recoveryPayload
	if len(encoded) == 0 || len(encoded) > MaxWalletFileBytes {
		return payload, walletV2FormatError("wallet file size is invalid")
	}
	if len(password) < 1 || len(password) > 1024 {
		return payload, ErrWalletUnlock
	}
	if expectedNetwork != core.MainNetMachineID && expectedNetwork != core.RegTestMachineID {
		return payload, walletV2FormatError("expected network is invalid")
	}
	if err := rejectDuplicateJSONKeys(encoded); err != nil {
		return payload, walletV2FormatError("wallet JSON is invalid")
	}
	var envelope v2Envelope
	if err := decodeStrictJSON(encoded, &envelope); err != nil {
		return payload, walletV2FormatError("wallet envelope is invalid")
	}
	if envelope.SchemaVersion != SchemaVersionV2 || envelope.Network != expectedNetwork {
		return payload, walletV2FormatError("wallet schema or network mismatch")
	}
	if err := validateV2KDFParams(envelope.KDF, true); err != nil {
		return payload, walletV2FormatError("wallet KDF is invalid")
	}
	if envelope.Cipher.Name != v2CipherName {
		return payload, walletV2FormatError("wallet cipher is invalid")
	}
	salt, err := decodeCanonicalBase64(envelope.KDF.Salt, v2SaltBytes)
	if err != nil {
		return payload, walletV2FormatError("wallet salt is invalid")
	}
	nonce, err := decodeCanonicalBase64(envelope.Cipher.Nonce, v2NonceBytes)
	if err != nil {
		clear(salt)
		return payload, walletV2FormatError("wallet nonce is invalid")
	}
	ciphertext, err := decodeCanonicalBase64Range(envelope.Cipher.Ciphertext, chacha20poly1305.Overhead+1, MaxWalletFileBytes)
	if err != nil {
		clear(salt)
		clear(nonce)
		return payload, walletV2FormatError("wallet ciphertext is invalid")
	}
	associatedData, err := v2AssociatedData(envelope)
	if err != nil {
		clear(salt)
		clear(nonce)
		clear(ciphertext)
		return payload, walletV2FormatError("wallet header is invalid")
	}
	key := argon2.IDKey(password, salt, envelope.KDF.Time, envelope.KDF.MemoryKiB, envelope.KDF.Parallelism, chacha20poly1305.KeySize)
	clear(salt)
	aead, err := chacha20poly1305.NewX(key)
	clear(key)
	if err != nil {
		clear(nonce)
		clear(ciphertext)
		return payload, ErrWalletUnlock
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, associatedData)
	clear(nonce)
	clear(ciphertext)
	if err != nil {
		return payload, ErrWalletUnlock
	}
	defer clear(plaintext)
	if err := rejectDuplicateJSONKeys(plaintext); err != nil {
		return payload, walletV2FormatError("wallet payload JSON is invalid")
	}
	if err := decodeStrictJSON(plaintext, &payload); err != nil {
		return recoveryPayload{}, walletV2FormatError("wallet payload is invalid")
	}
	if err := validateRecoveryPayload(payload); err != nil {
		clear(payload.Entropy)
		return recoveryPayload{}, walletV2FormatError("wallet recovery payload is invalid")
	}
	return payload, nil
}

func validateRecoveryPayload(payload recoveryPayload) error {
	if len(payload.Entropy) != RecoveryEntropyBytes {
		return errors.New("wallet recovery entropy length is invalid")
	}
	if payload.AddressCount < 1 || payload.AddressCount > uint32(MaxWalletKeys) {
		return errors.New("wallet recovery address count is invalid")
	}
	if payload.Derivation != RecoveryDerivationID {
		return errors.New("wallet recovery derivation is invalid")
	}
	return nil
}

func validateV2KDFParams(params v2KDFParams, requireSalt bool) error {
	if params.Name != v2KDFName ||
		params.Time < minV2KDFTime || params.Time > maxV2KDFTime ||
		params.MemoryKiB < minV2KDFMemoryKiB || params.MemoryKiB > maxV2KDFMemoryKiB ||
		params.Parallelism < 1 || params.Parallelism > maxV2KDFParallel {
		return errors.New("wallet KDF parameters are out of range")
	}
	if requireSalt {
		if _, err := decodeCanonicalBase64(params.Salt, v2SaltBytes); err != nil {
			return err
		}
	} else if params.Salt != "" {
		return errors.New("wallet KDF salt must be generated internally")
	}
	return nil
}

func v2AssociatedData(envelope v2Envelope) ([]byte, error) {
	header := v2AuthenticatedHeader{
		SchemaVersion: envelope.SchemaVersion,
		Network:       envelope.Network,
		KDF:           envelope.KDF,
		CipherName:    envelope.Cipher.Name,
	}
	return json.Marshal(header)
}

func decodeCanonicalBase64(value string, size int) ([]byte, error) {
	return decodeCanonicalBase64Range(value, size, size)
}

func decodeCanonicalBase64Range(value string, minimum, maximum int) ([]byte, error) {
	decoded, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil || len(decoded) < minimum || len(decoded) > maximum || base64.RawStdEncoding.EncodeToString(decoded) != value {
		clear(decoded)
		return nil, errors.New("invalid canonical base64")
	}
	return decoded, nil
}

func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := walkJSONValue(decoder); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is invalid")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[key] = struct{}{}
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func walletV2FormatError(message string) error {
	return fmt.Errorf("%w: %s", ErrWalletV2Format, message)
}
