package core

import (
	"crypto/ed25519"
	"testing"
)

func TestWalletViewSeparatesSpendableImmatureAndMempoolActivity(t *testing.T) {
	c := testChain(t)
	aliceKey, alicePKH := keyAndPKH(t)
	_, bobPKH := keyAndPKH(t)
	for i := int64(0); i < RegTest.CoinbaseMaturity+1; i++ {
		mineOne(t, c, alicePKH)
	}

	spendableBefore := c.UTXOsForPKH(alicePKH)
	var source OutPoint
	var sourceEntry UTXOEntry
	var totalBefore int64
	for outpoint, entry := range spendableBefore {
		totalBefore += entry.Value
		if sourceEntry.Value == 0 {
			source, sourceEntry = outpoint, entry
		}
	}
	const fee = int64(1_000)
	sendAmount := sourceEntry.Value / 3
	tx := &Tx{
		Version: 1,
		Ins:     []TxIn{{Prev: source}},
		Outs: []TxOut{
			{Value: sendAmount, PubKeyHash: bobPKH},
			{Value: sourceEntry.Value - sendAmount - fee, PubKeyHash: alicePKH},
		},
	}
	if err := tx.Sign([]ed25519.PrivateKey{aliceKey}); err != nil {
		t.Fatal(err)
	}
	if err := c.AcceptTx(tx); err != nil {
		t.Fatal(err)
	}

	view, err := c.WalletViewForPKHs([][20]byte{alicePKH}, MaxWalletActivityLimit)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Complete || view.Network != RegTestMachineID || view.Tip.Network != RegTestMachineID {
		t.Fatalf("unexpected view identity: %#v", view)
	}
	if view.SpendableUnits != totalBefore-sourceEntry.Value {
		t.Fatalf("spendable units = %d, want %d", view.SpendableUnits, totalBefore-sourceEntry.Value)
	}
	for _, output := range view.SpendableOutputs {
		if output.OutPoint == source {
			t.Fatal("mempool-spent output remained spendable")
		}
	}
	if len(view.ImmatureOutputs) == 0 || view.ImmatureUnits == 0 {
		t.Fatalf("immature rewards missing: %#v", view)
	}
	for _, output := range view.ImmatureOutputs {
		if output.Confirmations >= RegTest.CoinbaseMaturity {
			t.Fatalf("mature reward returned as immature: %#v", output)
		}
	}
	if len(view.Activity) == 0 || len(view.Activity) > MaxWalletActivityLimit {
		t.Fatalf("activity length = %d, want 1..%d", len(view.Activity), MaxWalletActivityLimit)
	}
	item := view.Activity[0]
	if item.TxID != tx.ID() || item.Kind != WalletActivitySent || item.Status != WalletActivityMempool {
		t.Fatalf("first activity = %#v, want mempool send %s", item, tx.ID())
	}
	if item.NetUnits != -(sendAmount+fee) || item.BlockHeight != -1 || item.Confirmations != 0 {
		t.Fatalf("mempool send amounts = %#v", item)
	}
}

func TestWalletViewClassifiesConfirmedReceiveSendAndCleanup(t *testing.T) {
	c := testChain(t)
	aliceKey, alicePKH := keyAndPKH(t)
	_, bobPKH := keyAndPKH(t)
	for i := int64(0); i < RegTest.CoinbaseMaturity+2; i++ {
		mineOne(t, c, alicePKH)
	}

	utxos := c.UTXOsForPKH(alicePKH)
	var sendSource OutPoint
	var sendEntry UTXOEntry
	for outpoint, entry := range utxos {
		sendSource, sendEntry = outpoint, entry
		break
	}
	const sendFee = int64(1_000)
	sendAmount := sendEntry.Value / 4
	sendTx := &Tx{
		Version: 1,
		Ins:     []TxIn{{Prev: sendSource}},
		Outs: []TxOut{
			{Value: sendAmount, PubKeyHash: bobPKH},
			{Value: sendEntry.Value - sendAmount - sendFee, PubKeyHash: alicePKH},
		},
	}
	if err := sendTx.Sign([]ed25519.PrivateKey{aliceKey}); err != nil {
		t.Fatal(err)
	}
	if err := c.AcceptTx(sendTx); err != nil {
		t.Fatal(err)
	}
	mineOne(t, c, alicePKH)

	utxos = c.UTXOsForPKH(alicePKH)
	cleanupInputs := make([]TxIn, 0, 2)
	cleanupKeys := make([]ed25519.PrivateKey, 0, 2)
	var cleanupTotal int64
	for outpoint, entry := range utxos {
		cleanupInputs = append(cleanupInputs, TxIn{Prev: outpoint})
		cleanupKeys = append(cleanupKeys, aliceKey)
		cleanupTotal += entry.Value
		if len(cleanupInputs) == 2 {
			break
		}
	}
	const cleanupFee = int64(2_000)
	cleanupTx := &Tx{
		Version: 1,
		Ins:     cleanupInputs,
		Outs:    []TxOut{{Value: cleanupTotal - cleanupFee, PubKeyHash: alicePKH}},
	}
	if err := cleanupTx.Sign(cleanupKeys); err != nil {
		t.Fatal(err)
	}
	if err := c.AcceptTx(cleanupTx); err != nil {
		t.Fatal(err)
	}
	mineOne(t, c, alicePKH)

	aliceView, err := c.WalletViewForPKHs([][20]byte{alicePKH}, MaxWalletActivityLimit)
	if err != nil {
		t.Fatal(err)
	}
	sent := requireWalletActivity(t, aliceView.Activity, sendTx.ID())
	if sent.Kind != WalletActivitySent || sent.Status != WalletActivityConfirmed || sent.NetUnits != -(sendAmount+sendFee) {
		t.Fatalf("send activity = %#v", sent)
	}
	cleanup := requireWalletActivity(t, aliceView.Activity, cleanupTx.ID())
	if cleanup.Kind != WalletActivityCleanup || cleanup.Status != WalletActivityConfirmed || cleanup.NetUnits != -cleanupFee {
		t.Fatalf("cleanup activity = %#v", cleanup)
	}
	if cleanup.TransactionIndex != 1 || cleanup.Confirmations != 1 {
		t.Fatalf("cleanup confirmation metadata = %#v", cleanup)
	}

	bobView, err := c.WalletViewForPKHs([][20]byte{bobPKH}, MaxWalletActivityLimit)
	if err != nil {
		t.Fatal(err)
	}
	received := requireWalletActivity(t, bobView.Activity, sendTx.ID())
	if received.Kind != WalletActivityReceived || received.NetUnits != sendAmount || received.Status != WalletActivityConfirmed {
		t.Fatalf("receive activity = %#v", received)
	}
}

func TestWalletViewLimitsDuplicateOwnersAndEmptyWallet(t *testing.T) {
	c := testChain(t)
	_, pkh := keyAndPKH(t)
	mineOne(t, c, pkh)

	if _, err := c.WalletViewForPKHs([][20]byte{pkh}, -1); err == nil {
		t.Fatal("negative activity limit accepted")
	}
	if _, err := c.WalletViewForPKHs([][20]byte{pkh}, MaxWalletActivityLimit+1); err == nil {
		t.Fatal("oversized activity limit accepted")
	}
	limited, err := c.WalletViewForPKHs([][20]byte{pkh}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited.Activity) != 1 {
		t.Fatalf("limited activity length = %d, want 1", len(limited.Activity))
	}
	duplicateView, err := c.WalletViewForPKHs([][20]byte{pkh, pkh}, MaxWalletActivityLimit)
	if err != nil {
		t.Fatalf("duplicate owner should be harmless: %v", err)
	}
	if len(duplicateView.ImmatureOutputs) != 1 || duplicateView.ImmatureOutputs[0].OwnerIndex != 0 {
		t.Fatalf("duplicate-owner view = %#v", duplicateView)
	}

	empty, err := c.WalletViewForPKHs(nil, MaxWalletActivityLimit)
	if err != nil {
		t.Fatal(err)
	}
	if !empty.Complete || empty.Network != RegTestMachineID || len(empty.SpendableOutputs) != 0 || len(empty.Activity) != 0 {
		t.Fatalf("empty wallet view = %#v", empty)
	}
}

func TestWalletViewRejectsMoneyOverflowInPersistedCanonicalData(t *testing.T) {
	c := testChain(t)
	_, pkh := keyAndPKH(t)
	block := mineOne(t, c, pkh)

	c.mu.Lock()
	blockIndex := c.index[block.Header.ID()]
	blockIndex.block.Txs[0].Outs[0].Value = MaxMoneyUnits + 1
	c.mu.Unlock()

	_, err := c.WalletViewForPKHs([][20]byte{pkh}, 0)
	if err == nil {
		t.Fatal("corrupted canonical value was accepted")
	}
}

func requireWalletActivity(t *testing.T, activity []WalletActivityItem, txID Hash32) WalletActivityItem {
	t.Helper()
	for _, item := range activity {
		if item.TxID == txID {
			return item
		}
	}
	t.Fatalf("activity does not contain %s", txID)
	return WalletActivityItem{}
}
