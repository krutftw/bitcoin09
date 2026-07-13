package wallet

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bip39 "github.com/cosmos/go-bip39"
	"github.com/krutftw/bitcoin09/core"
)

func TestWalletV2CreateReopenAddAddressAndRestore(t *testing.T) {
	originalRandom := walletV2Random
	originalKDF := walletV2KDFParams
	walletV2Random = bytes.NewReader(bytes.Repeat([]byte{0x73}, 512))
	walletV2KDFParams = v2TestKDFParams()
	t.Cleanup(func() {
		walletV2Random = originalRandom
		walletV2KDFParams = originalKDF
	})

	path := filepath.Join(t.TempDir(), "wallet-v2.json")
	password := []byte("correct horse battery staple")
	created, phrase, err := CreateV2(path, core.RegTestMachineID, password)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Fields(phrase)) != RecoveryWordCount {
		t.Fatalf("recovery word count = %d", len(strings.Fields(phrase)))
	}
	firstAddresses, err := created.AddressesE()
	if err != nil || len(firstAddresses) != 1 {
		t.Fatalf("created addresses = %v, %v", firstAddresses, err)
	}
	secondAddress, err := created.NewAddress()
	if err != nil {
		t.Fatal(err)
	}
	allAddresses, err := created.AddressesE()
	if err != nil || len(allAddresses) != 2 || allAddresses[1] != secondAddress {
		t.Fatalf("addresses after add = %v, %v", allAddresses, err)
	}
	created.Close()

	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(phrase)) || bytes.Contains(encoded, []byte(`"keys"`)) {
		t.Fatal("Wallet V2 file exposed phrase or V1 key array")
	}
	if !bytes.Contains(encoded, []byte(`"schema_version": 2`)) {
		t.Fatalf("Wallet V2 schema is missing: %s", encoded)
	}

	reopened, err := OpenV2(path, core.RegTestMachineID, password)
	if err != nil {
		t.Fatal(err)
	}
	reopenedAddresses, err := reopened.AddressesE()
	if err != nil || !equalStrings(reopenedAddresses, allAddresses) {
		t.Fatalf("reopened addresses = %v, %v", reopenedAddresses, err)
	}
	reopened.Close()
	if _, err := OpenV2(path, core.RegTestMachineID, []byte("wrong password")); !errors.Is(err, ErrWalletUnlock) {
		t.Fatalf("wrong password error = %v", err)
	}
	if _, err := Open(path, core.RegTestMachineID); !errors.Is(err, ErrWalletUnlock) {
		t.Fatalf("passwordless V2 open error = %v", err)
	}

	restoredPath := filepath.Join(t.TempDir(), "restored-v2.json")
	restored, err := RestoreV2(restoredPath, core.RegTestMachineID, []byte("new local password"), phrase, 2)
	if err != nil {
		t.Fatal(err)
	}
	restoredAddresses, err := restored.AddressesE()
	if err != nil || !equalStrings(restoredAddresses, allAddresses) {
		t.Fatalf("restored addresses = %v, %v", restoredAddresses, err)
	}
	restored.Close()
}

func TestWalletV2APINeverRewritesLegacyWallet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-wallet.json")
	legacy, err := LoadOrCreateForNetwork(path, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	legacyAddresses := legacy.Addresses()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenV2(path, core.RegTestMachineID, []byte("wallet-password")); !errors.Is(err, ErrWalletV2Format) {
		t.Fatalf("OpenV2 legacy error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) || !equalStrings(legacy.Addresses(), legacyAddresses) {
		t.Fatal("Wallet V2 API changed the legacy wallet")
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func TestSLIP10Ed25519MatchesOfficialVector(t *testing.T) {
	seed, err := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		path      []uint32
		private   string
		chainCode string
	}{
		{nil, "2b4be7f19ee27bbf30c667b642d5f4aa69fd169872f8fc3059c08ebae2eb19e7", "90046a93de5380a72b5e45010748567d5ea02bbf6522f979e05c0d8d8ca9fffb"},
		{[]uint32{0}, "68e0fe46dfb67e368c75379acec591dad19df3cde26e63b93a8e704f1dade7a3", "8b59aa11380b624e81507a27fedda59fea6d0b779a778918a2fd3590e16e9c69"},
		{[]uint32{0, 1}, "b1d0bad404bf35da785a64ca1ac54b2617211d2777696fbffaf208f746ae84f2", "a320425f77d1b5c2505a6b1b27382b37368ee640e3557c315416801243552f14"},
		{[]uint32{0, 1, 2}, "92a5b23c0b8a99e37d07df3fb9966917f5d06e02ddbd909c7e184371463e9fc9", "2e69929e00b5ab250f49c3fb1c12f252de4fed2c1db88387094a0f8c4c9ccd6c"},
	}
	for _, tc := range tests {
		private, chainCode, err := deriveSLIP10Ed25519(seed, tc.path)
		if err != nil {
			t.Fatalf("derive path %v: %v", tc.path, err)
		}
		if hex.EncodeToString(private[:]) != tc.private || hex.EncodeToString(chainCode[:]) != tc.chainCode {
			t.Fatalf("path %v private=%x chain=%x", tc.path, private, chainCode)
		}
	}
}

func TestRecoveryPhraseUses24WordEnglishBIP39AndRoundTripsEntropy(t *testing.T) {
	entropy := make([]byte, RecoveryEntropyBytes)
	phrase, err := recoveryPhraseFromEntropy(entropy)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(strings.Repeat("abandon ", 23)) + " art"
	if phrase != want {
		t.Fatalf("zero-entropy phrase = %q", phrase)
	}
	if len(strings.Fields(phrase)) != RecoveryWordCount {
		t.Fatalf("word count = %d", len(strings.Fields(phrase)))
	}
	recovered, err := recoveryEntropyFromPhrase(phrase)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recovered, entropy) {
		t.Fatalf("entropy mismatch: %x", recovered)
	}
	if _, err := recoveryEntropyFromPhrase(strings.Replace(phrase, "abandon", "notaword", 1)); err == nil {
		t.Fatal("invalid recovery word was accepted")
	}
}

func TestRecoveryWordListIsPrivateFromDependencyMutation(t *testing.T) {
	original := bip39.WordList[0]
	bip39.WordList[0] = "changed"
	t.Cleanup(func() { bip39.WordList[0] = original })
	phrase, err := recoveryPhraseFromEntropy(make([]byte, RecoveryEntropyBytes))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(strings.Repeat("abandon ", 23)) + " art"
	if phrase != want {
		t.Fatalf("phrase changed through dependency global: %q", phrase)
	}
}

func TestRecoveryAddressSequenceIsDeterministicAndNetworkSeparated(t *testing.T) {
	entropy := make([]byte, RecoveryEntropyBytes)
	for i := range entropy {
		entropy[i] = byte(i)
	}
	phrase, err := recoveryPhraseFromEntropy(entropy)
	if err != nil {
		t.Fatal(err)
	}
	mainZero, err := deriveRecoveryPrivateKey(phrase, core.MainNetMachineID, 0)
	if err != nil {
		t.Fatal(err)
	}
	mainZeroAgain, err := deriveRecoveryPrivateKey(phrase, core.MainNetMachineID, 0)
	if err != nil {
		t.Fatal(err)
	}
	mainOne, err := deriveRecoveryPrivateKey(phrase, core.MainNetMachineID, 1)
	if err != nil {
		t.Fatal(err)
	}
	regtestZero, err := deriveRecoveryPrivateKey(phrase, core.RegTestMachineID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(mainZero)
	defer clear(mainZeroAgain)
	defer clear(mainOne)
	defer clear(regtestZero)

	if !bytes.Equal(mainZero, mainZeroAgain) {
		t.Fatal("same phrase, network, and index produced different keys")
	}
	if bytes.Equal(mainZero, mainOne) || bytes.Equal(mainZero, regtestZero) {
		t.Fatal("address index or network did not separate derived keys")
	}
	for _, key := range []ed25519.PrivateKey{mainZero, mainOne, regtestZero} {
		if _, err := core.DecodeAddress(addressForKey(key)); err != nil {
			t.Fatalf("derived invalid BTC09 address: %v", err)
		}
	}
}

func TestRecoveryPayloadEncryptionRoundTripAndPlaintextExclusion(t *testing.T) {
	entropy := bytes.Repeat([]byte{0x5a}, RecoveryEntropyBytes)
	phrase, err := recoveryPhraseFromEntropy(entropy)
	if err != nil {
		t.Fatal(err)
	}
	payload := recoveryPayload{
		Entropy:      append([]byte(nil), entropy...),
		AddressCount: 7,
		Derivation:   RecoveryDerivationID,
	}
	randomness := bytes.NewReader(bytes.Repeat([]byte{0x42}, v2SaltBytes+v2NonceBytes))
	encoded, err := sealRecoveryPayload(
		core.MainNetMachineID,
		[]byte("correct horse battery staple"),
		payload,
		randomness,
		v2TestKDFParams(),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		phrase,
		hex.EncodeToString(entropy),
		base64.RawStdEncoding.EncodeToString(entropy),
	} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("wallet envelope contains recovery material %q", forbidden)
		}
	}
	opened, err := openRecoveryPayload(
		encoded,
		[]byte("correct horse battery staple"),
		core.MainNetMachineID,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(opened.Entropy)
	if !bytes.Equal(opened.Entropy, entropy) || opened.AddressCount != 7 || opened.Derivation != RecoveryDerivationID {
		t.Fatalf("unexpected opened payload: %+v", opened)
	}
}

func TestRecoveryPayloadWrongPasswordTamperAndNetworkFailClosed(t *testing.T) {
	payload := recoveryPayload{
		Entropy:      bytes.Repeat([]byte{0x33}, RecoveryEntropyBytes),
		AddressCount: 1,
		Derivation:   RecoveryDerivationID,
	}
	encoded, err := sealRecoveryPayload(
		core.RegTestMachineID,
		[]byte("wallet-password"),
		payload,
		bytes.NewReader(bytes.Repeat([]byte{0x24}, v2SaltBytes+v2NonceBytes)),
		v2TestKDFParams(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openRecoveryPayload(encoded, []byte("wrong-password"), core.RegTestMachineID); !errors.Is(err, ErrWalletUnlock) {
		t.Fatalf("wrong password error = %v", err)
	}
	if _, err := openRecoveryPayload(encoded, []byte("wallet-password"), core.MainNetMachineID); !errors.Is(err, ErrWalletV2Format) {
		t.Fatalf("wrong network error = %v", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	cipher := envelope["cipher"].(map[string]any)
	ciphertext, err := base64.RawStdEncoding.DecodeString(cipher["ciphertext"].(string))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext[len(ciphertext)-1] ^= 1
	cipher["ciphertext"] = base64.RawStdEncoding.EncodeToString(ciphertext)
	tampered, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openRecoveryPayload(tampered, []byte("wallet-password"), core.RegTestMachineID); !errors.Is(err, ErrWalletUnlock) {
		t.Fatalf("tamper error = %v", err)
	}
}

func TestRecoveryPayloadRejectsHostileKDFBeforeArgon2(t *testing.T) {
	payload := recoveryPayload{
		Entropy:      bytes.Repeat([]byte{0x11}, RecoveryEntropyBytes),
		AddressCount: 1,
		Derivation:   RecoveryDerivationID,
	}
	encoded, err := sealRecoveryPayload(
		core.MainNetMachineID,
		[]byte("wallet-password"),
		payload,
		bytes.NewReader(bytes.Repeat([]byte{0x55}, v2SaltBytes+v2NonceBytes)),
		v2TestKDFParams(),
	)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope["kdf"].(map[string]any)["memory_kib"] = float64(maxV2KDFMemoryKiB + 1)
	hostile, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openRecoveryPayload(hostile, []byte("wallet-password"), core.MainNetMachineID); !errors.Is(err, ErrWalletV2Format) {
		t.Fatalf("hostile KDF error = %v", err)
	}
}

func TestRecoverySeedFromEntropyMatchesBIP39(t *testing.T) {
	entropy := make([]byte, RecoveryEntropyBytes)
	phrase, err := recoveryPhraseFromEntropy(entropy)
	if err != nil {
		t.Fatal(err)
	}
	want := bip39.NewSeed(phrase, "")
	defer clear(want)
	got, err := recoverySeedFromEntropy(entropy)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(got)
	if !bytes.Equal(got, want) {
		t.Fatalf("BIP39 seed mismatch: got %x want %x", got, want)
	}
}

func TestRecoveryAddressVectorIsStable(t *testing.T) {
	phrase := strings.TrimSpace(strings.Repeat("abandon ", 23)) + " art"
	key, err := deriveRecoveryPrivateKey(phrase, core.MainNetMachineID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	const want = "4sUTrPpm9QvzkMrARSjFzVzHyJzpw6MbYA"
	if got := addressForKey(key); got != want {
		t.Fatalf("mainnet m/9009'/0'/0'/0' address = %q, want %q", got, want)
	}
}

func TestRecoveryEnvelopeRejectsAmbiguousOrMalformedJSON(t *testing.T) {
	encoded := testRecoveryEnvelope(t)
	var envelope map[string]any
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}

	mutate := func(t *testing.T, change func(map[string]any)) []byte {
		t.Helper()
		var clone map[string]any
		if err := json.Unmarshal(encoded, &clone); err != nil {
			t.Fatal(err)
		}
		change(clone)
		result, err := json.Marshal(clone)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	tests := []struct {
		name string
		data []byte
	}{
		{"duplicate root key", []byte(`{"schema_version":2,"schema_version":2}`)},
		{"unknown root key", mutate(t, func(value map[string]any) { value["unexpected"] = true })},
		{"trailing JSON", append(append([]byte(nil), encoded...), []byte(` {}`)...)},
		{"padded salt", mutate(t, func(value map[string]any) {
			kdf := value["kdf"].(map[string]any)
			kdf["salt"] = kdf["salt"].(string) + "="
		})},
		{"short nonce", mutate(t, func(value map[string]any) { value["cipher"].(map[string]any)["nonce"] = "AA" })},
		{"empty ciphertext", mutate(t, func(value map[string]any) { value["cipher"].(map[string]any)["ciphertext"] = "" })},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := openRecoveryPayload(tc.data, []byte("wallet-password"), core.MainNetMachineID); !errors.Is(err, ErrWalletV2Format) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRecoveryEnvelopeAuthenticatesKDFMetadata(t *testing.T) {
	encoded := testRecoveryEnvelope(t)
	var envelope map[string]any
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope["kdf"].(map[string]any)["time"] = float64(minV2KDFTime + 1)
	tampered, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openRecoveryPayload(tampered, []byte("wallet-password"), core.MainNetMachineID); !errors.Is(err, ErrWalletUnlock) {
		t.Fatalf("metadata tamper error = %v", err)
	}
}

func TestWalletV2CloseLocksTheHandle(t *testing.T) {
	originalKDF := walletV2KDFParams
	walletV2KDFParams = v2TestKDFParams()
	t.Cleanup(func() { walletV2KDFParams = originalKDF })

	w, _, err := CreateV2(filepath.Join(t.TempDir(), "wallet-v2.json"), core.RegTestMachineID, []byte("wallet-password"))
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	if _, err := w.AddressesE(); !errors.Is(err, ErrWalletUnlock) {
		t.Fatalf("AddressesE after Close error = %v", err)
	}
	if _, err := w.NewAddress(); !errors.Is(err, ErrWalletUnlock) {
		t.Fatalf("NewAddress after Close error = %v", err)
	}
}

func TestWalletV2RecoveryPhraseRequiresUnlockedHandle(t *testing.T) {
	originalKDF := walletV2KDFParams
	walletV2KDFParams = v2TestKDFParams()
	t.Cleanup(func() { walletV2KDFParams = originalKDF })

	w, createdPhrase, err := CreateV2(filepath.Join(t.TempDir(), "wallet-v2.json"), core.RegTestMachineID, []byte("wallet-password"))
	if err != nil {
		t.Fatal(err)
	}
	phrase, err := w.RecoveryPhrase()
	if err != nil || phrase != createdPhrase {
		t.Fatalf("RecoveryPhrase = %q, %v", phrase, err)
	}
	w.Close()
	if _, err := w.RecoveryPhrase(); !errors.Is(err, ErrWalletUnlock) {
		t.Fatalf("RecoveryPhrase after Close error = %v", err)
	}
}

func testRecoveryEnvelope(t *testing.T) []byte {
	t.Helper()
	encoded, err := sealRecoveryPayload(
		core.MainNetMachineID,
		[]byte("wallet-password"),
		recoveryPayload{
			Entropy:      bytes.Repeat([]byte{0x4d}, RecoveryEntropyBytes),
			AddressCount: 1,
			Derivation:   RecoveryDerivationID,
		},
		bytes.NewReader(bytes.Repeat([]byte{0x28}, v2SaltBytes+v2NonceBytes)),
		v2TestKDFParams(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
