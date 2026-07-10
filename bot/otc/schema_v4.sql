PRAGMA foreign_keys = ON;
PRAGMA recursive_triggers = ON;
CREATE TABLE IF NOT EXISTS schema_meta (
  id INTEGER PRIMARY KEY CHECK(id = 1),
  version INTEGER NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS users (
  user_id INTEGER PRIMARY KEY,
  username TEXT NOT NULL CHECK(
    instr(username, char(0)) = 0 AND
    length(CAST(username AS BLOB)) BETWEEN 1 AND 128
  ),
  wallet_addr TEXT CHECK(wallet_addr IS NULL OR (
    instr(wallet_addr, char(0)) = 0 AND
    length(CAST(wallet_addr AS BLOB)) BETWEEN 1 AND 128
  )),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS orders (
  order_id INTEGER PRIMARY KEY AUTOINCREMENT,
  side TEXT NOT NULL CHECK(side IN ('buy','sell')),
  maker_id INTEGER NOT NULL,
  maker_name TEXT NOT NULL CHECK(
    instr(maker_name, char(0)) = 0 AND
    length(CAST(maker_name AS BLOB)) BETWEEN 1 AND 128
  ),
  buyer_id INTEGER,
  buyer_name TEXT CHECK(buyer_name IS NULL OR (
    instr(buyer_name, char(0)) = 0 AND
    length(CAST(buyer_name AS BLOB)) BETWEEN 1 AND 128
  )),
  seller_id INTEGER,
  seller_name TEXT CHECK(seller_name IS NULL OR (
    instr(seller_name, char(0)) = 0 AND
    length(CAST(seller_name AS BLOB)) BETWEEN 1 AND 128
  )),
  net_amount_units INTEGER NOT NULL
    CHECK(net_amount_units BETWEEN 1 AND 2100000000000000),
  network_fee_units INTEGER NOT NULL
    CHECK(network_fee_units BETWEEN 0 AND 2100000000000000),
  service_fee_units INTEGER NOT NULL
    CHECK(service_fee_units BETWEEN 0 AND 2100000000000000),
  deposit_required_units INTEGER NOT NULL
    CHECK(deposit_required_units BETWEEN 1 AND 2100000000000000),
  total_price TEXT NOT NULL CHECK(
    instr(total_price, char(0)) = 0 AND
    length(CAST(total_price AS BLOB)) BETWEEN 1 AND 37 AND
    total_price NOT GLOB '*[^0-9.]*' AND
    substr(total_price, 1, 1) != '.' AND
    substr(total_price, -1, 1) != '.' AND
    (instr(total_price, '.') = 0 OR
      instr(substr(total_price, instr(total_price, '.') + 1), '.') = 0) AND
    (substr(total_price, 1, 1) != '0' OR
      (length(total_price) > 1 AND substr(total_price, 2, 1) = '.')) AND
    ((instr(total_price, '.') = 0 AND length(total_price) <= 18) OR
      (instr(total_price, '.') BETWEEN 2 AND 19 AND
       length(total_price) - instr(total_price, '.') BETWEEN 1 AND 18)) AND
    total_price GLOB '*[1-9]*'
  ),
  settlement_asset TEXT NOT NULL CHECK(
    instr(settlement_asset, char(0)) = 0 AND
    length(CAST(settlement_asset AS BLOB)) BETWEEN 2 AND 12 AND
    settlement_asset NOT GLOB '*[^A-Z0-9._-]*'
  ),
  settlement_network TEXT CHECK(settlement_network IS NULL OR (
    instr(settlement_network, char(0)) = 0 AND
    length(CAST(settlement_network AS BLOB)) BETWEEN 1 AND 48 AND
    settlement_network NOT GLOB '*[^A-Za-z0-9._ -]*'
  )),
  payment_method TEXT NOT NULL CHECK(
    instr(payment_method, char(0)) = 0 AND
    length(CAST(payment_method AS BLOB)) BETWEEN 1 AND 80 AND
    payment_method NOT GLOB '*[^ -~]*'
  ),
  state TEXT NOT NULL CHECK(state IN (
    'awaiting_deposit','open','matched','disputed','release_reserved',
    'refund_reserved','broadcast','completed','refunded','cancelled',
    'deposit_expired','recovery_hold','transfer_failed_safe','transfer_uncertain'
  )),
  deposit_addr TEXT CHECK(deposit_addr IS NULL OR (
    instr(deposit_addr, char(0)) = 0 AND
    length(CAST(deposit_addr AS BLOB)) BETWEEN 1 AND 128
  )),
  buyer_confirmed INTEGER NOT NULL DEFAULT 0 CHECK(buyer_confirmed IN (0,1)),
  seller_confirmed INTEGER NOT NULL DEFAULT 0 CHECK(seller_confirmed IN (0,1)),
  deposit_deadline INTEGER,
  matched_at INTEGER,
  trade_deadline INTEGER,
  disputed_at INTEGER,
  completed_at INTEGER,
  funded_at INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(order_id, deposit_addr),
  CHECK(deposit_required_units =
    net_amount_units + network_fee_units + service_fee_units),
  CHECK((buyer_id IS NULL AND buyer_name IS NULL) OR
        (buyer_id IS NOT NULL AND buyer_name IS NOT NULL)),
  CHECK((seller_id IS NULL AND seller_name IS NULL) OR
        (seller_id IS NOT NULL AND seller_name IS NOT NULL)),
  CHECK(
    (side = 'sell' AND seller_id = maker_id AND seller_name = maker_name) OR
    (side = 'buy' AND buyer_id = maker_id AND buyer_name = maker_name)
  ),
  CHECK(buyer_id IS NULL OR seller_id IS NULL OR buyer_id != seller_id),
  FOREIGN KEY(maker_id) REFERENCES users(user_id),
  FOREIGN KEY(buyer_id) REFERENCES users(user_id),
  FOREIGN KEY(seller_id) REFERENCES users(user_id)
) STRICT;
CREATE TABLE IF NOT EXISTS deposit_scans (
  scan_id INTEGER PRIMARY KEY AUTOINCREMENT,
  network TEXT NOT NULL CHECK(network IN ('btc09-mainnet','btc09-regtest')),
  address TEXT NOT NULL CHECK(
    instr(address, char(0)) = 0 AND
    length(CAST(address AS BLOB)) BETWEEN 1 AND 128
  ),
  tip_hash TEXT NOT NULL CHECK(
    instr(tip_hash, char(0)) = 0 AND
    length(CAST(tip_hash AS BLOB)) = 64 AND
    tip_hash NOT GLOB '*[^0-9a-f]*'
  ),
  tip_height INTEGER NOT NULL CHECK(tip_height >= 0),
  observed_at INTEGER NOT NULL,
  UNIQUE(scan_id, network, address)
) STRICT;
CREATE TABLE IF NOT EXISTS deposit_credits (
  credit_id INTEGER PRIMARY KEY AUTOINCREMENT,
  order_id INTEGER NOT NULL REFERENCES orders(order_id),
  network TEXT NOT NULL CHECK(network IN ('btc09-mainnet','btc09-regtest')),
  txid TEXT NOT NULL CHECK(
    instr(txid, char(0)) = 0 AND
    length(CAST(txid AS BLOB)) = 64 AND
    txid NOT GLOB '*[^0-9a-f]*'
  ),
  vout INTEGER NOT NULL CHECK(vout >= 0),
  deposit_addr TEXT NOT NULL CHECK(
    instr(deposit_addr, char(0)) = 0 AND
    length(CAST(deposit_addr AS BLOB)) BETWEEN 1 AND 128
  ),
  amount_units INTEGER NOT NULL
    CHECK(amount_units BETWEEN 1 AND 2100000000000000),
  block_hash TEXT NOT NULL CHECK(
    instr(block_hash, char(0)) = 0 AND
    length(CAST(block_hash AS BLOB)) = 64 AND
    block_hash NOT GLOB '*[^0-9a-f]*'
  ),
  block_height INTEGER NOT NULL CHECK(block_height >= 0),
  confirmations INTEGER NOT NULL CHECK(confirmations >= 1),
  coinbase INTEGER NOT NULL CHECK(coinbase IN (0,1)),
  mature INTEGER NOT NULL CHECK(mature IN (0,1)),
  current_best_chain INTEGER NOT NULL CHECK(current_best_chain IN (0,1)),
  spent_by_txid TEXT,
  spent_by_vin INTEGER,
  spent_by_block_hash TEXT,
  spent_by_block_height INTEGER,
  credited_at INTEGER,
  main_units INTEGER NOT NULL DEFAULT 0 CHECK(main_units >= 0),
  recovery_units INTEGER NOT NULL DEFAULT 0 CHECK(recovery_units >= 0),
  recovery_reason TEXT CHECK(
    recovery_reason IS NULL OR
    recovery_reason IN ('excess','late','cancelled_partial')
  ),
  first_seen_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  last_seen_scan_id INTEGER NOT NULL REFERENCES deposit_scans(scan_id),
  last_checked_scan_id INTEGER NOT NULL REFERENCES deposit_scans(scan_id),
  UNIQUE(network, txid, vout),
  UNIQUE(credit_id, order_id),
  CHECK(main_units + recovery_units <= amount_units),
  CHECK(credited_at IS NOT NULL OR main_units + recovery_units = 0),
  CHECK(credited_at IS NULL OR main_units + recovery_units = amount_units),
  CHECK(
    (recovery_units = 0 AND recovery_reason IS NULL) OR
    (recovery_units > 0 AND recovery_reason IS NOT NULL)
  ),
  CHECK(last_seen_at >= first_seen_at),
  CHECK(credited_at IS NULL OR credited_at >= first_seen_at),
  CHECK(current_best_chain = 0 OR last_seen_scan_id = last_checked_scan_id),
  CHECK(
    (spent_by_txid IS NULL AND spent_by_vin IS NULL AND
     spent_by_block_hash IS NULL AND spent_by_block_height IS NULL) OR
    (spent_by_txid IS NOT NULL AND spent_by_vin IS NOT NULL AND
     spent_by_block_hash IS NOT NULL AND spent_by_block_height IS NOT NULL AND
     instr(spent_by_txid, char(0)) = 0 AND
     length(CAST(spent_by_txid AS BLOB)) = 64 AND
     spent_by_txid NOT GLOB '*[^0-9a-f]*' AND
     spent_by_vin >= 0 AND
     instr(spent_by_block_hash, char(0)) = 0 AND
     length(CAST(spent_by_block_hash AS BLOB)) = 64 AND
     spent_by_block_hash NOT GLOB '*[^0-9a-f]*' AND
     spent_by_block_height >= 0)
  ),
  FOREIGN KEY(order_id, deposit_addr)
    REFERENCES orders(order_id, deposit_addr),
  FOREIGN KEY(last_seen_scan_id, network, deposit_addr)
    REFERENCES deposit_scans(scan_id, network, address),
  FOREIGN KEY(last_checked_scan_id, network, deposit_addr)
    REFERENCES deposit_scans(scan_id, network, address)
) STRICT;
CREATE TABLE IF NOT EXISTS transfers (
  transfer_id INTEGER PRIMARY KEY AUTOINCREMENT,
  operation_key TEXT NOT NULL UNIQUE CHECK(
    instr(operation_key, char(0)) = 0 AND
    length(CAST(operation_key AS BLOB)) BETWEEN 1 AND 160 AND
    operation_key NOT GLOB '*[^a-z0-9:_-]*'
  ),
  order_id INTEGER REFERENCES orders(order_id),
  wallet_scope TEXT NOT NULL DEFAULT 'escrow' CHECK(wallet_scope = 'escrow'),
  kind TEXT NOT NULL CHECK(kind IN (
    'release','refund','resolve_buyer','resolve_seller','recovery_refund',
    'fee_withdrawal'
  )),
  is_main_outcome INTEGER NOT NULL CHECK(is_main_outcome IN (0,1)),
  state TEXT NOT NULL CHECK(state IN (
    'queued','reserved','prepared','broadcast','confirmed','failed_safe',
    'uncertain','cancelled'
  )),
  amount_units INTEGER NOT NULL
    CHECK(amount_units BETWEEN 1 AND 2100000000000000),
  network_fee_units INTEGER NOT NULL
    CHECK(network_fee_units BETWEEN 0 AND 2100000000000000),
  earned_fee_units INTEGER NOT NULL DEFAULT 0
    CHECK(earned_fee_units BETWEEN 0 AND 2100000000000000),
  destination TEXT NOT NULL CHECK(
    instr(destination, char(0)) = 0 AND
    length(CAST(destination AS BLOB)) BETWEEN 1 AND 128
  ),
  txid TEXT CHECK(
    txid IS NULL OR (
      instr(txid, char(0)) = 0 AND
      length(CAST(txid AS BLOB)) = 64 AND
      txid NOT GLOB '*[^0-9a-f]*'
    )
  ),
  signed_tx_hex TEXT CHECK(
    signed_tx_hex IS NULL OR
    (instr(signed_tx_hex, char(0)) = 0 AND
     length(CAST(signed_tx_hex AS BLOB)) BETWEEN 2 AND 20000 AND
     length(CAST(signed_tx_hex AS BLOB)) % 2 = 0 AND
     signed_tx_hex NOT GLOB '*[^0-9a-f]*')
  ),
  prepared_tip_hash TEXT CHECK(
    prepared_tip_hash IS NULL OR (
      instr(prepared_tip_hash, char(0)) = 0 AND
      length(CAST(prepared_tip_hash AS BLOB)) = 64 AND
      prepared_tip_hash NOT GLOB '*[^0-9a-f]*'
    )
  ),
  prepared_tip_height INTEGER CHECK(
    prepared_tip_height IS NULL OR prepared_tip_height >= 0
  ),
  result_class TEXT CHECK(
    result_class IS NULL OR
    result_class IN ('broadcast','safe_to_retry','uncertain')
  ),
  error_text TEXT CHECK(error_text IS NULL OR (
    instr(error_text, char(0)) = 0 AND
    length(CAST(error_text AS BLOB)) <= 500
  )),
  attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count >= 0),
  reserved_at INTEGER,
  signed_at INTEGER,
  broadcast_at INTEGER,
  confirmed_at INTEGER,
  confirmed_block_hash TEXT CHECK(
    confirmed_block_hash IS NULL OR
    (instr(confirmed_block_hash, char(0)) = 0 AND
     length(CAST(confirmed_block_hash AS BLOB)) = 64 AND
     confirmed_block_hash NOT GLOB '*[^0-9a-f]*')
  ),
  confirmed_block_height INTEGER,
  confirmations INTEGER NOT NULL DEFAULT 0 CHECK(confirmations >= 0),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  CHECK(
    (kind = 'fee_withdrawal' AND order_id IS NULL) OR
    (kind != 'fee_withdrawal' AND order_id IS NOT NULL)
  ),
  CHECK(
    (is_main_outcome = 1 AND kind IN (
      'release','refund','resolve_buyer','resolve_seller'
    )) OR
    (is_main_outcome = 0 AND kind IN ('recovery_refund','fee_withdrawal'))
  ),
  CHECK(earned_fee_units = 0 OR kind IN ('release','resolve_buyer')),
  CHECK(confirmed_block_height IS NULL OR confirmed_block_height >= 0),
  CHECK(
    state NOT IN ('queued','reserved','failed_safe','cancelled') OR
    (txid IS NULL AND signed_tx_hex IS NULL AND signed_at IS NULL AND
     prepared_tip_hash IS NULL AND prepared_tip_height IS NULL)
  ),
  CHECK(
    state NOT IN ('prepared','broadcast','confirmed','uncertain') OR
    (txid IS NOT NULL AND signed_tx_hex IS NOT NULL AND signed_at IS NOT NULL AND
     prepared_tip_hash IS NOT NULL AND prepared_tip_height IS NOT NULL)
  ),
  CHECK(state != 'broadcast' OR txid IS NOT NULL),
  CHECK(
    state != 'confirmed' OR
    (txid IS NOT NULL AND confirmed_at IS NOT NULL AND
     confirmed_block_hash IS NOT NULL AND confirmed_block_height IS NOT NULL AND
     confirmations >= 1)
  ),
  UNIQUE(transfer_id, order_id)
) STRICT;
CREATE TABLE IF NOT EXISTS transfer_credit_allocations (
  transfer_id INTEGER NOT NULL REFERENCES transfers(transfer_id),
  credit_id INTEGER NOT NULL REFERENCES deposit_credits(credit_id),
  order_id INTEGER NOT NULL REFERENCES orders(order_id),
  bucket TEXT NOT NULL CHECK(bucket IN ('main','recovery')),
  units INTEGER NOT NULL CHECK(units BETWEEN 1 AND 2100000000000000),
  PRIMARY KEY(transfer_id, credit_id, bucket),
  FOREIGN KEY(transfer_id, order_id)
    REFERENCES transfers(transfer_id, order_id),
  FOREIGN KEY(credit_id, order_id)
    REFERENCES deposit_credits(credit_id, order_id)
) STRICT;
CREATE INDEX IF NOT EXISTS orders_by_state ON orders(state, updated_at);
CREATE UNIQUE INDEX IF NOT EXISTS one_order_per_deposit_address
ON orders(deposit_addr) WHERE deposit_addr IS NOT NULL;
CREATE INDEX IF NOT EXISTS deposit_scans_by_address
ON deposit_scans(network, address, scan_id DESC);
CREATE INDEX IF NOT EXISTS deposit_credits_by_order
ON deposit_credits(order_id, credited_at);
CREATE INDEX IF NOT EXISTS deposit_credits_by_address
ON deposit_credits(network, deposit_addr, current_best_chain);
CREATE UNIQUE INDEX IF NOT EXISTS one_main_outcome_per_order
ON transfers(order_id) WHERE is_main_outcome = 1;
CREATE UNIQUE INDEX IF NOT EXISTS one_transfer_per_txid
ON transfers(txid) WHERE txid IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS one_unfinished_recovery_per_order
ON transfers(order_id)
WHERE kind = 'recovery_refund' AND state NOT IN ('confirmed','cancelled');
CREATE UNIQUE INDEX IF NOT EXISTS one_wallet_send_in_flight
ON transfers(wallet_scope) WHERE state IN ('reserved','prepared','broadcast');
CREATE INDEX IF NOT EXISTS transfers_queue
ON transfers(state, created_at, transfer_id);
CREATE INDEX IF NOT EXISTS transfer_allocations_by_credit
ON transfer_credit_allocations(credit_id);
CREATE TRIGGER IF NOT EXISTS order_insert_invariant
BEFORE INSERT ON orders
WHEN NOT (
  NEW.side = 'sell'
  AND NEW.state = 'awaiting_deposit'
  AND NEW.seller_id = NEW.maker_id
  AND NEW.seller_name = NEW.maker_name
  AND NEW.buyer_id IS NULL
  AND NEW.buyer_name IS NULL
  AND NEW.deposit_addr IS NOT NULL
  AND NEW.buyer_confirmed = 0
  AND NEW.seller_confirmed = 0
) AND NOT (
  NEW.side = 'buy'
  AND NEW.state = 'open'
  AND NEW.buyer_id = NEW.maker_id
  AND NEW.buyer_name = NEW.maker_name
  AND NEW.seller_id IS NULL
  AND NEW.seller_name IS NULL
  AND NEW.deposit_addr IS NULL
  AND NEW.buyer_confirmed = 0
  AND NEW.seller_confirmed = 0
)
BEGIN
  SELECT RAISE(ABORT, 'invalid initial order roles or state');
END;
CREATE TRIGGER IF NOT EXISTS order_quote_immutable
BEFORE UPDATE ON orders
WHEN NEW.side IS NOT OLD.side
  OR NEW.maker_id IS NOT OLD.maker_id
  OR NEW.maker_name IS NOT OLD.maker_name
  OR NEW.net_amount_units IS NOT OLD.net_amount_units
  OR NEW.network_fee_units IS NOT OLD.network_fee_units
  OR NEW.service_fee_units IS NOT OLD.service_fee_units
  OR NEW.deposit_required_units IS NOT OLD.deposit_required_units
  OR NEW.total_price IS NOT OLD.total_price
  OR NEW.settlement_asset IS NOT OLD.settlement_asset
  OR NEW.settlement_network IS NOT OLD.settlement_network
  OR NEW.payment_method IS NOT OLD.payment_method
  OR NEW.created_at IS NOT OLD.created_at
  OR (OLD.deposit_addr IS NOT NULL AND NEW.deposit_addr IS NOT OLD.deposit_addr)
BEGIN
  SELECT RAISE(ABORT, 'order quote and deposit identity are immutable');
END;
CREATE TRIGGER IF NOT EXISTS order_participant_transition_guard
BEFORE UPDATE ON orders
WHEN (
  OLD.side = 'sell' AND NOT (
    (NEW.buyer_id IS OLD.buyer_id AND NEW.buyer_name IS OLD.buyer_name) OR
    (OLD.buyer_id IS NULL AND OLD.buyer_name IS NULL
      AND NEW.buyer_id IS NOT NULL AND NEW.buyer_name IS NOT NULL
      AND NEW.buyer_id != OLD.maker_id
      AND OLD.state = 'open' AND NEW.state = 'matched'
      AND OLD.buyer_confirmed = 0 AND OLD.seller_confirmed = 0
      AND NEW.buyer_confirmed = 0 AND NEW.seller_confirmed = 0) OR
    (OLD.buyer_id IS NOT NULL AND OLD.buyer_name IS NOT NULL
      AND NEW.buyer_id IS NULL AND NEW.buyer_name IS NULL
      AND OLD.state = 'matched' AND NEW.state = 'open'
      AND OLD.buyer_confirmed = 0 AND OLD.seller_confirmed = 0
      AND NEW.buyer_confirmed = 0 AND NEW.seller_confirmed = 0)
  )
) OR (
  OLD.side = 'buy' AND NOT (
    (NEW.seller_id IS OLD.seller_id AND NEW.seller_name IS OLD.seller_name) OR
    (OLD.seller_id IS NULL AND OLD.seller_name IS NULL
      AND NEW.seller_id IS NOT NULL AND NEW.seller_name IS NOT NULL
      AND NEW.seller_id != OLD.maker_id
      AND OLD.state = 'open' AND NEW.state = 'awaiting_deposit'
      AND OLD.deposit_addr IS NULL AND NEW.deposit_addr IS NOT NULL
      AND OLD.buyer_confirmed = 0 AND OLD.seller_confirmed = 0
      AND NEW.buyer_confirmed = 0 AND NEW.seller_confirmed = 0)
  )
)
BEGIN
  SELECT RAISE(ABORT, 'invalid order participant transition');
END;
CREATE TRIGGER IF NOT EXISTS order_deposit_assignment_guard
BEFORE UPDATE OF deposit_addr ON orders
WHEN NEW.deposit_addr IS NOT OLD.deposit_addr AND NOT (
  OLD.side = 'buy'
  AND OLD.deposit_addr IS NULL
  AND NEW.deposit_addr IS NOT NULL
  AND OLD.state = 'open'
  AND NEW.state = 'awaiting_deposit'
  AND OLD.seller_id IS NULL
  AND OLD.seller_name IS NULL
  AND NEW.seller_id IS NOT NULL
  AND NEW.seller_name IS NOT NULL
)
BEGIN
  SELECT RAISE(ABORT, 'deposit address may only attach with first WTB seller');
END;
CREATE TRIGGER IF NOT EXISTS order_confirmation_guard
BEFORE UPDATE OF buyer_confirmed, seller_confirmed ON orders
WHEN NEW.buyer_confirmed != OLD.buyer_confirmed
  OR NEW.seller_confirmed != OLD.seller_confirmed
BEGIN
  SELECT CASE WHEN OLD.state != 'matched'
      OR NEW.buyer_confirmed < OLD.buyer_confirmed
      OR NEW.seller_confirmed < OLD.seller_confirmed
      OR NEW.buyer_confirmed + NEW.seller_confirmed
         != OLD.buyer_confirmed + OLD.seller_confirmed + 1
      OR (NEW.buyer_confirmed + NEW.seller_confirmed = 1
          AND NEW.state != 'matched')
      OR (NEW.buyer_confirmed + NEW.seller_confirmed = 2
          AND NEW.state != 'release_reserved')
    THEN RAISE(ABORT, 'invalid payment confirmation transition') END;
END;
CREATE TRIGGER IF NOT EXISTS order_state_machine
BEFORE UPDATE OF state ON orders
WHEN NEW.state IS NOT OLD.state AND NOT (
  (OLD.side = 'sell' AND OLD.state = 'awaiting_deposit'
    AND (
      (NEW.state = 'open' AND COALESCE((
        SELECT SUM(c.main_units) FROM deposit_credits c
        WHERE c.order_id = OLD.order_id AND c.credited_at IS NOT NULL
      ), 0) >= OLD.deposit_required_units) OR
      (NEW.state IN ('cancelled','deposit_expired') AND COALESCE((
        SELECT SUM(c.amount_units) FROM deposit_credits c
        WHERE c.order_id = OLD.order_id AND c.credited_at IS NOT NULL
      ), 0) = 0) OR
      (NEW.state = 'recovery_hold'
        AND OLD.buyer_confirmed = 0 AND OLD.seller_confirmed = 0
        AND COALESCE((SELECT SUM(c.amount_units) FROM deposit_credits c
          WHERE c.order_id = OLD.order_id AND c.credited_at IS NOT NULL), 0)
            BETWEEN 1 AND OLD.deposit_required_units - 1) OR
      (NEW.state = 'refund_reserved' AND EXISTS (
        SELECT 1 FROM transfers t WHERE t.order_id = OLD.order_id
          AND t.kind IN ('refund','recovery_refund') AND t.state = 'queued'
          AND COALESCE((SELECT SUM(a.units)
            FROM transfer_credit_allocations a
            WHERE a.transfer_id = t.transfer_id), 0)
              = t.amount_units + t.network_fee_units + t.earned_fee_units
      ))
    )) OR
  (OLD.side = 'sell' AND OLD.state = 'open'
    AND ((NEW.state = 'matched' AND NEW.buyer_id IS NOT NULL)
      OR (NEW.state = 'refund_reserved' AND EXISTS (
        SELECT 1 FROM transfers t WHERE t.order_id = OLD.order_id
          AND t.kind = 'refund' AND t.state = 'queued'
          AND COALESCE((SELECT SUM(a.units)
            FROM transfer_credit_allocations a
            WHERE a.transfer_id = t.transfer_id), 0)
              = t.amount_units + t.network_fee_units + t.earned_fee_units
      )))) OR
  (OLD.side = 'sell' AND OLD.state = 'matched'
    AND ((NEW.state = 'open' AND NEW.buyer_id IS NULL
          AND OLD.buyer_confirmed = 0 AND OLD.seller_confirmed = 0)
      OR (NEW.state = 'release_reserved'
          AND NEW.buyer_confirmed = 1 AND NEW.seller_confirmed = 1
          AND EXISTS (SELECT 1 FROM transfers t
            WHERE t.order_id = OLD.order_id AND t.is_main_outcome = 1
              AND t.kind = 'release' AND t.state = 'queued'
              AND COALESCE((SELECT SUM(a.units)
                FROM transfer_credit_allocations a
                WHERE a.transfer_id = t.transfer_id), 0)
                  = t.amount_units + t.network_fee_units + t.earned_fee_units))
      OR NEW.state = 'disputed'
      OR (NEW.state = 'refund_reserved'
          AND OLD.buyer_confirmed = 0 AND OLD.seller_confirmed = 0
          AND EXISTS (SELECT 1 FROM transfers t
            WHERE t.order_id = OLD.order_id AND t.is_main_outcome = 1
              AND t.kind = 'refund' AND t.state = 'queued'
              AND COALESCE((SELECT SUM(a.units)
                FROM transfer_credit_allocations a
                WHERE a.transfer_id = t.transfer_id), 0)
                  = t.amount_units + t.network_fee_units + t.earned_fee_units)))) OR
  (OLD.side = 'buy' AND OLD.state = 'open'
    AND ((NEW.state = 'awaiting_deposit' AND NEW.seller_id IS NOT NULL
          AND NEW.deposit_addr IS NOT NULL)
      OR NEW.state = 'cancelled')) OR
  (OLD.side = 'buy' AND OLD.state = 'awaiting_deposit'
    AND (
      (NEW.state = 'matched' AND COALESCE((
        SELECT SUM(c.main_units) FROM deposit_credits c
        WHERE c.order_id = OLD.order_id AND c.credited_at IS NOT NULL
      ), 0) >= OLD.deposit_required_units) OR
      (NEW.state IN ('cancelled','deposit_expired') AND COALESCE((
        SELECT SUM(c.amount_units) FROM deposit_credits c
        WHERE c.order_id = OLD.order_id AND c.credited_at IS NOT NULL
      ), 0) = 0) OR
      (NEW.state = 'recovery_hold'
        AND OLD.buyer_confirmed = 0 AND OLD.seller_confirmed = 0
        AND COALESCE((SELECT SUM(c.amount_units) FROM deposit_credits c
          WHERE c.order_id = OLD.order_id AND c.credited_at IS NOT NULL), 0)
            BETWEEN 1 AND OLD.deposit_required_units - 1) OR
      (NEW.state = 'refund_reserved' AND EXISTS (
        SELECT 1 FROM transfers t WHERE t.order_id = OLD.order_id
          AND t.kind IN ('refund','recovery_refund') AND t.state = 'queued'
          AND COALESCE((SELECT SUM(a.units)
            FROM transfer_credit_allocations a
            WHERE a.transfer_id = t.transfer_id), 0)
              = t.amount_units + t.network_fee_units + t.earned_fee_units
      ))
    )) OR
  (OLD.side = 'buy' AND OLD.state = 'matched'
    AND ((NEW.state = 'release_reserved'
          AND NEW.buyer_confirmed = 1 AND NEW.seller_confirmed = 1
          AND EXISTS (SELECT 1 FROM transfers t
            WHERE t.order_id = OLD.order_id AND t.is_main_outcome = 1
              AND t.kind = 'release' AND t.state = 'queued'
              AND COALESCE((SELECT SUM(a.units)
                FROM transfer_credit_allocations a
                WHERE a.transfer_id = t.transfer_id), 0)
                  = t.amount_units + t.network_fee_units + t.earned_fee_units))
      OR NEW.state = 'disputed'
      OR (NEW.state = 'refund_reserved'
          AND OLD.buyer_confirmed = 0 AND OLD.seller_confirmed = 0
          AND EXISTS (SELECT 1 FROM transfers t
            WHERE t.order_id = OLD.order_id AND t.is_main_outcome = 1
              AND t.kind = 'refund' AND t.state = 'queued'
              AND COALESCE((SELECT SUM(a.units)
                FROM transfer_credit_allocations a
                WHERE a.transfer_id = t.transfer_id), 0)
                  = t.amount_units + t.network_fee_units + t.earned_fee_units)))) OR
  (OLD.state = 'disputed' AND (
    (NEW.state = 'release_reserved' AND EXISTS (
      SELECT 1 FROM transfers t WHERE t.order_id = OLD.order_id
        AND t.kind = 'resolve_buyer' AND t.state = 'queued'
        AND COALESCE((SELECT SUM(a.units)
          FROM transfer_credit_allocations a
          WHERE a.transfer_id = t.transfer_id), 0)
            = t.amount_units + t.network_fee_units + t.earned_fee_units
    )) OR
    (NEW.state = 'refund_reserved' AND EXISTS (
      SELECT 1 FROM transfers t WHERE t.order_id = OLD.order_id
        AND t.kind = 'resolve_seller' AND t.state = 'queued'
        AND COALESCE((SELECT SUM(a.units)
          FROM transfer_credit_allocations a
          WHERE a.transfer_id = t.transfer_id), 0)
            = t.amount_units + t.network_fee_units + t.earned_fee_units
    ))
  )) OR
  (OLD.state = 'release_reserved'
    AND NEW.state IN ('broadcast','completed','transfer_failed_safe','transfer_uncertain')) OR
  (OLD.state = 'refund_reserved'
    AND NEW.state IN ('broadcast','refunded','transfer_failed_safe','transfer_uncertain')) OR
  (OLD.state = 'broadcast'
    AND NEW.state IN ('completed','refunded','transfer_uncertain')) OR
  (OLD.state = 'transfer_failed_safe' AND (
    (NEW.state = 'release_reserved' AND EXISTS (
      SELECT 1 FROM transfers t WHERE t.order_id = OLD.order_id
        AND t.kind IN ('release','resolve_buyer') AND t.state = 'queued'
        AND COALESCE((SELECT SUM(a.units)
          FROM transfer_credit_allocations a
          WHERE a.transfer_id = t.transfer_id), 0)
            = t.amount_units + t.network_fee_units + t.earned_fee_units
    )) OR
    (NEW.state = 'refund_reserved' AND EXISTS (
      SELECT 1 FROM transfers t WHERE t.order_id = OLD.order_id
        AND t.kind IN ('refund','resolve_seller','recovery_refund')
        AND t.state = 'queued'
        AND COALESCE((SELECT SUM(a.units)
          FROM transfer_credit_allocations a
          WHERE a.transfer_id = t.transfer_id), 0)
            = t.amount_units + t.network_fee_units + t.earned_fee_units
    ))
  )) OR
  (OLD.state = 'transfer_uncertain'
    AND NEW.state IN ('broadcast','completed','refunded')) OR
  (OLD.state IN ('completed','refunded')
    AND NEW.state = 'transfer_uncertain') OR
  (OLD.state = 'recovery_hold' AND NEW.state = 'refund_reserved'
    AND EXISTS (SELECT 1 FROM transfers t WHERE t.order_id = OLD.order_id
      AND t.kind = 'recovery_refund' AND t.state = 'queued'
      AND COALESCE((SELECT SUM(a.units)
        FROM transfer_credit_allocations a
        WHERE a.transfer_id = t.transfer_id), 0)
          = t.amount_units + t.network_fee_units + t.earned_fee_units))
)
BEGIN
  SELECT RAISE(ABORT, 'invalid order state transition');
END;
CREATE TRIGGER IF NOT EXISTS order_delete_block
BEFORE DELETE ON orders
BEGIN
  SELECT RAISE(ABORT, 'orders are append-only');
END;
CREATE TRIGGER IF NOT EXISTS deposit_scan_update_block
BEFORE UPDATE ON deposit_scans
BEGIN
  SELECT RAISE(ABORT, 'deposit scans are append-only');
END;
CREATE TRIGGER IF NOT EXISTS deposit_scan_delete_block
BEFORE DELETE ON deposit_scans
BEGIN
  SELECT RAISE(ABORT, 'deposit scans are append-only');
END;
CREATE TRIGGER IF NOT EXISTS deposit_credit_identity_immutable
BEFORE UPDATE ON deposit_credits
WHEN NEW.order_id IS NOT OLD.order_id
  OR NEW.network IS NOT OLD.network
  OR NEW.txid IS NOT OLD.txid
  OR NEW.vout IS NOT OLD.vout
  OR NEW.deposit_addr IS NOT OLD.deposit_addr
  OR NEW.amount_units IS NOT OLD.amount_units
  OR NEW.coinbase IS NOT OLD.coinbase
  OR NEW.first_seen_at IS NOT OLD.first_seen_at
  OR (OLD.credited_at IS NOT NULL AND NEW.credited_at IS NOT OLD.credited_at)
BEGIN
  SELECT RAISE(ABORT, 'deposit credit identity is immutable');
END;
CREATE TRIGGER IF NOT EXISTS deposit_credit_classification_guard
BEFORE UPDATE OF credited_at, main_units, recovery_units, recovery_reason
ON deposit_credits
WHEN NOT (
  NEW.credited_at IS OLD.credited_at
  AND NEW.main_units = OLD.main_units
  AND NEW.recovery_units = OLD.recovery_units
  AND NEW.recovery_reason IS OLD.recovery_reason
) AND NOT (
  OLD.credited_at IS NULL
  AND NEW.credited_at IS NOT NULL
  AND OLD.main_units = 0
  AND OLD.recovery_units = 0
  AND NEW.main_units + NEW.recovery_units = OLD.amount_units
) AND NOT (
  OLD.credited_at IS NOT NULL
  AND NEW.credited_at = OLD.credited_at
  AND OLD.main_units > 0
  AND NEW.main_units = 0
  AND NEW.recovery_units = OLD.main_units + OLD.recovery_units
  AND NEW.recovery_reason = 'cancelled_partial'
  AND EXISTS (
    SELECT 1 FROM orders o
    WHERE o.order_id = OLD.order_id
      AND o.state IN ('refund_reserved','recovery_hold')
      AND o.buyer_confirmed = 0
      AND o.seller_confirmed = 0
      AND COALESCE((
        SELECT SUM(c.amount_units)
        FROM deposit_credits c
        WHERE c.order_id = o.order_id AND c.credited_at IS NOT NULL
      ), 0) < o.deposit_required_units
  )
  AND NOT EXISTS (
    SELECT 1 FROM transfer_credit_allocations a
    WHERE a.credit_id = OLD.credit_id AND a.bucket = 'main'
  )
  AND NOT EXISTS (
    SELECT 1 FROM transfers t
    WHERE t.order_id = OLD.order_id AND t.is_main_outcome = 1
  )
)
BEGIN
  SELECT RAISE(ABORT, 'invalid deposit credit classification change');
END;
CREATE TRIGGER IF NOT EXISTS deposit_credit_observation_guard
BEFORE UPDATE ON deposit_credits
WHEN NEW.block_hash IS NOT OLD.block_hash
  OR NEW.block_height IS NOT OLD.block_height
  OR NEW.confirmations IS NOT OLD.confirmations
  OR NEW.mature IS NOT OLD.mature
  OR NEW.current_best_chain IS NOT OLD.current_best_chain
  OR NEW.spent_by_txid IS NOT OLD.spent_by_txid
  OR NEW.spent_by_vin IS NOT OLD.spent_by_vin
  OR NEW.spent_by_block_hash IS NOT OLD.spent_by_block_hash
  OR NEW.spent_by_block_height IS NOT OLD.spent_by_block_height
  OR NEW.last_seen_at IS NOT OLD.last_seen_at
  OR NEW.last_seen_scan_id IS NOT OLD.last_seen_scan_id
  OR NEW.last_checked_scan_id IS NOT OLD.last_checked_scan_id
BEGIN
  SELECT CASE WHEN NEW.last_checked_scan_id <= OLD.last_checked_scan_id
    THEN RAISE(ABORT, 'credit observation requires a newer scan') END;
  SELECT CASE WHEN NEW.last_seen_at < OLD.last_seen_at
    THEN RAISE(ABORT, 'credit last-seen time cannot move backwards') END;
END;
CREATE TRIGGER IF NOT EXISTS deposit_credit_bucket_capacity_guard
BEFORE UPDATE OF main_units, recovery_units ON deposit_credits
BEGIN
  SELECT CASE WHEN NEW.main_units < COALESCE((
    SELECT SUM(a.units) FROM transfer_credit_allocations a
    WHERE a.credit_id = OLD.credit_id AND a.bucket = 'main'
  ), 0) THEN RAISE(ABORT, 'main credit capacity is allocated') END;
  SELECT CASE WHEN NEW.recovery_units < COALESCE((
    SELECT SUM(a.units) FROM transfer_credit_allocations a
    WHERE a.credit_id = OLD.credit_id AND a.bucket = 'recovery'
  ), 0) THEN RAISE(ABORT, 'recovery credit capacity is allocated') END;
END;
CREATE TRIGGER IF NOT EXISTS deposit_credit_delete_block
BEFORE DELETE ON deposit_credits
BEGIN
  SELECT RAISE(ABORT, 'deposit credits are append-only');
END;
CREATE TRIGGER IF NOT EXISTS transfer_operation_key_lifetime_guard
BEFORE INSERT ON transfers
WHEN EXISTS (
  SELECT 1 FROM transfers t WHERE t.operation_key = NEW.operation_key
)
BEGIN
  SELECT RAISE(ABORT, 'transfer operation key already exists');
END;
CREATE TRIGGER IF NOT EXISTS transfer_insert_must_queue
BEFORE INSERT ON transfers
WHEN NEW.state != 'queued'
  OR NEW.attempt_count != 0
  OR NEW.reserved_at IS NOT NULL
  OR NEW.signed_at IS NOT NULL
  OR NEW.broadcast_at IS NOT NULL
  OR NEW.confirmed_at IS NOT NULL
  OR NEW.confirmed_block_hash IS NOT NULL
  OR NEW.confirmed_block_height IS NOT NULL
  OR NEW.confirmations != 0
BEGIN
  SELECT RAISE(ABORT, 'new transfer must be a clean queued operation');
END;
CREATE TRIGGER IF NOT EXISTS transfer_economics_on_insert
BEFORE INSERT ON transfers
BEGIN
  SELECT CASE WHEN NEW.kind IN ('release','resolve_buyer') AND NOT EXISTS (
    SELECT 1 FROM orders o JOIN users u ON u.user_id = o.buyer_id
    WHERE o.order_id = NEW.order_id
      AND NEW.operation_key = 'order:' || o.order_id || ':main'
      AND NEW.amount_units = o.net_amount_units
      AND NEW.network_fee_units = o.network_fee_units
      AND NEW.earned_fee_units = o.service_fee_units
      AND NEW.destination = u.wallet_addr
  ) THEN RAISE(ABORT, 'invalid buyer outcome economics') END;
  SELECT CASE WHEN NEW.kind IN ('refund','resolve_seller') AND NOT EXISTS (
    SELECT 1 FROM orders o JOIN users u ON u.user_id = o.seller_id
    WHERE o.order_id = NEW.order_id
      AND NEW.operation_key = 'order:' || o.order_id || ':main'
      AND NEW.amount_units = o.net_amount_units + o.service_fee_units
      AND NEW.network_fee_units = o.network_fee_units
      AND NEW.earned_fee_units = 0
      AND NEW.destination = u.wallet_addr
  ) THEN RAISE(ABORT, 'invalid seller outcome economics') END;
  SELECT CASE WHEN NEW.kind = 'recovery_refund' AND NOT EXISTS (
    SELECT 1 FROM orders o JOIN users u ON u.user_id = o.seller_id
    WHERE o.order_id = NEW.order_id
      AND NEW.operation_key = 'order:' || o.order_id || ':recovery:' || (
        SELECT MAX(c.credit_id) FROM deposit_credits c
        WHERE c.order_id = o.order_id AND c.credited_at IS NOT NULL
          AND c.recovery_units > COALESCE((
            SELECT SUM(a.units) FROM transfer_credit_allocations a
            WHERE a.credit_id = c.credit_id AND a.bucket = 'recovery'
          ), 0)
      )
      AND NEW.network_fee_units = o.network_fee_units
      AND NEW.earned_fee_units = 0
      AND NEW.destination = u.wallet_addr
      AND NEW.amount_units + NEW.network_fee_units =
        COALESCE((SELECT SUM(c.recovery_units)
                  FROM deposit_credits c
                  WHERE c.order_id = NEW.order_id
                    AND c.credited_at IS NOT NULL), 0)
        - COALESCE((SELECT SUM(a.units)
                    FROM transfer_credit_allocations a
                    JOIN transfers t ON t.transfer_id = a.transfer_id
                    WHERE t.order_id = NEW.order_id
                      AND a.bucket = 'recovery'), 0)
  ) THEN RAISE(ABORT, 'invalid recovery economics') END;
  SELECT CASE WHEN NEW.kind = 'fee_withdrawal' AND (
    NEW.earned_fee_units != 0 OR
    NEW.amount_units + NEW.network_fee_units >
      COALESCE((SELECT SUM(earned_fee_units) FROM transfers
                WHERE state = 'confirmed'
                  AND kind IN ('release','resolve_buyer')), 0)
      - COALESCE((SELECT SUM(amount_units + network_fee_units) FROM transfers
                  WHERE kind = 'fee_withdrawal' AND state != 'cancelled'), 0)
  ) THEN RAISE(ABORT, 'fee withdrawal exceeds earned revenue') END;
END;
CREATE TRIGGER IF NOT EXISTS transfer_economics_immutable
BEFORE UPDATE ON transfers
WHEN NEW.operation_key IS NOT OLD.operation_key
  OR NEW.order_id IS NOT OLD.order_id
  OR NEW.wallet_scope IS NOT OLD.wallet_scope
  OR NEW.kind IS NOT OLD.kind
  OR NEW.is_main_outcome IS NOT OLD.is_main_outcome
  OR NEW.amount_units IS NOT OLD.amount_units
  OR NEW.network_fee_units IS NOT OLD.network_fee_units
  OR NEW.earned_fee_units IS NOT OLD.earned_fee_units
  OR NEW.destination IS NOT OLD.destination
BEGIN
  SELECT RAISE(ABORT, 'transfer economics are immutable');
END;
CREATE TRIGGER IF NOT EXISTS signed_transfer_immutable
BEFORE UPDATE ON transfers
WHEN OLD.signed_tx_hex IS NOT NULL AND (
  NEW.txid IS NOT OLD.txid OR
  NEW.signed_tx_hex IS NOT OLD.signed_tx_hex OR
  NEW.signed_at IS NOT OLD.signed_at OR
  NEW.prepared_tip_hash IS NOT OLD.prepared_tip_hash OR
  NEW.prepared_tip_height IS NOT OLD.prepared_tip_height
)
BEGIN
  SELECT RAISE(ABORT, 'prepared transaction identity is immutable');
END;
CREATE TRIGGER IF NOT EXISTS transfer_timeline_guard
BEFORE UPDATE ON transfers
BEGIN
  SELECT CASE WHEN NEW.created_at IS NOT OLD.created_at
    OR NEW.updated_at < OLD.updated_at
    THEN RAISE(ABORT, 'invalid transfer timestamps') END;
  SELECT CASE WHEN NEW.attempt_count != OLD.attempt_count +
    CASE WHEN OLD.state = 'queued' AND NEW.state = 'reserved' THEN 1 ELSE 0 END
    THEN RAISE(ABORT, 'invalid transfer attempt count') END;
  SELECT CASE WHEN OLD.state = 'queued' AND NEW.state = 'reserved' AND (
      NEW.reserved_at IS NULL OR NEW.reserved_at < OLD.updated_at
    ) THEN RAISE(ABORT, 'claim requires reservation time') END;
  SELECT CASE WHEN NEW.reserved_at IS NOT OLD.reserved_at AND NOT (
      OLD.state = 'queued' AND NEW.state = 'reserved' AND OLD.reserved_at IS NULL
    ) AND NOT (
      OLD.state = 'failed_safe' AND NEW.state = 'queued'
      AND NEW.reserved_at IS NULL
    ) THEN RAISE(ABORT, 'invalid reservation time change') END;
  SELECT CASE WHEN OLD.state = 'reserved' AND NEW.state = 'prepared' AND (
      NEW.signed_at IS NULL OR NEW.txid IS NULL OR NEW.signed_tx_hex IS NULL OR
      NEW.prepared_tip_hash IS NULL OR NEW.prepared_tip_height IS NULL
    ) THEN RAISE(ABORT, 'prepare requires complete durable identity') END;
  SELECT CASE WHEN OLD.signed_tx_hex IS NULL AND NEW.signed_tx_hex IS NOT NULL
      AND NOT (OLD.state = 'reserved' AND NEW.state = 'prepared')
    THEN RAISE(ABORT, 'signed identity may only attach on prepare') END;
  SELECT CASE WHEN OLD.broadcast_at IS NOT NULL
      AND NEW.broadcast_at IS NOT OLD.broadcast_at
    THEN RAISE(ABORT, 'broadcast time is immutable') END;
  SELECT CASE WHEN NEW.state = 'broadcast' AND OLD.state != 'broadcast'
      AND NEW.broadcast_at IS NULL
    THEN RAISE(ABORT, 'trusted-node observation time required') END;
  SELECT CASE WHEN OLD.broadcast_at IS NULL AND NEW.broadcast_at IS NOT NULL
      AND NEW.state != 'broadcast'
    THEN RAISE(ABORT, 'broadcast time requires broadcast state') END;
END;
CREATE TRIGGER IF NOT EXISTS transfer_state_machine
BEFORE UPDATE OF state ON transfers
WHEN NEW.state IS NOT OLD.state AND NOT (
  (OLD.state = 'queued' AND NEW.state = 'reserved') OR
  (OLD.state = 'reserved' AND NEW.state IN ('prepared','failed_safe')) OR
  (OLD.state = 'prepared' AND NEW.state IN ('broadcast','confirmed','uncertain')) OR
  (OLD.state = 'broadcast' AND NEW.state IN ('confirmed','uncertain')) OR
  (OLD.state = 'uncertain' AND NEW.state IN ('broadcast','confirmed')) OR
  (OLD.state = 'confirmed' AND NEW.state = 'uncertain') OR
  (OLD.state = 'failed_safe' AND NEW.state = 'queued') OR
  (OLD.state = 'failed_safe' AND NEW.state = 'cancelled'
    AND OLD.kind = 'fee_withdrawal')
)
BEGIN
  SELECT RAISE(ABORT, 'invalid transfer state transition');
END;
CREATE TRIGGER IF NOT EXISTS allocation_insert_guard
BEFORE INSERT ON transfer_credit_allocations
BEGIN
  SELECT CASE WHEN NOT EXISTS (
    SELECT 1 FROM transfers t
    WHERE t.transfer_id = NEW.transfer_id
      AND t.order_id = NEW.order_id
      AND t.kind != 'fee_withdrawal'
      AND t.state = 'queued'
      AND ((t.is_main_outcome = 1 AND NEW.bucket = 'main') OR
           (t.kind = 'recovery_refund' AND NEW.bucket = 'recovery'))
  ) THEN RAISE(ABORT, 'invalid transfer allocation target') END;
  SELECT CASE WHEN NEW.units + COALESCE((
    SELECT SUM(a.units) FROM transfer_credit_allocations a
    WHERE a.credit_id = NEW.credit_id AND a.bucket = NEW.bucket
  ), 0) > COALESCE((
    SELECT CASE NEW.bucket
      WHEN 'main' THEN c.main_units ELSE c.recovery_units END
    FROM deposit_credits c
    WHERE c.credit_id = NEW.credit_id
      AND c.order_id = NEW.order_id
      AND c.credited_at IS NOT NULL
  ), -1) THEN RAISE(ABORT, 'credit bucket over-allocation') END;
END;
CREATE TRIGGER IF NOT EXISTS allocation_immutable_update
BEFORE UPDATE ON transfer_credit_allocations
BEGIN
  SELECT RAISE(ABORT, 'transfer allocations are immutable');
END;
CREATE TRIGGER IF NOT EXISTS allocation_immutable_delete
BEFORE DELETE ON transfer_credit_allocations
BEGIN
  SELECT RAISE(ABORT, 'transfer allocations are immutable');
END;
CREATE TRIGGER IF NOT EXISTS transfer_delete_block
BEFORE DELETE ON transfers
BEGIN
  SELECT RAISE(ABORT, 'transfers are append-only');
END;
CREATE TRIGGER IF NOT EXISTS transfer_claim_guard
BEFORE UPDATE OF state ON transfers
WHEN OLD.state = 'queued' AND NEW.state = 'reserved'
BEGIN
  SELECT CASE WHEN EXISTS (
    SELECT 1 FROM transfers WHERE state = 'uncertain'
  ) THEN RAISE(ABORT, 'uncertain transfer blocks wallet claim') END;
  SELECT CASE WHEN OLD.kind != 'fee_withdrawal' AND NOT EXISTS (
    SELECT 1 FROM orders o
    WHERE o.order_id = OLD.order_id AND (
      (OLD.kind = 'release' AND o.state = 'release_reserved'
        AND o.buyer_confirmed = 1 AND o.seller_confirmed = 1) OR
      (OLD.kind = 'refund' AND o.state = 'refund_reserved'
        AND o.buyer_confirmed = 0 AND o.seller_confirmed = 0) OR
      (OLD.kind = 'resolve_buyer' AND o.state = 'release_reserved') OR
      (OLD.kind = 'resolve_seller' AND o.state = 'refund_reserved') OR
      (OLD.kind = 'recovery_refund' AND o.state IN (
        'refund_reserved','completed','refunded','cancelled','deposit_expired'
      ))
    )
  ) THEN RAISE(ABORT, 'transfer is not authorized by order state') END;
  SELECT CASE WHEN COALESCE((
    SELECT SUM(units) FROM transfer_credit_allocations
    WHERE transfer_id = OLD.transfer_id
  ), 0) != CASE WHEN OLD.kind = 'fee_withdrawal' THEN 0
                ELSE OLD.amount_units + OLD.network_fee_units
                     + OLD.earned_fee_units END
  THEN RAISE(ABORT, 'transfer allocations do not balance') END;
  SELECT CASE WHEN EXISTS (
    SELECT 1
    FROM (
      SELECT credit_id, bucket, SUM(units) AS allocated_units
      FROM transfer_credit_allocations
      GROUP BY credit_id, bucket
    ) a
    JOIN deposit_credits c ON c.credit_id = a.credit_id
    WHERE a.allocated_units > CASE a.bucket
      WHEN 'main' THEN c.main_units ELSE c.recovery_units END
  ) THEN RAISE(ABORT, 'allocated credit capacity changed') END;
END;
CREATE TRIGGER IF NOT EXISTS transfer_confirmation_guard
BEFORE UPDATE OF state ON transfers
WHEN NEW.state = 'confirmed' AND OLD.state != 'confirmed'
BEGIN
  SELECT CASE WHEN COALESCE((
    SELECT SUM(units) FROM transfer_credit_allocations
    WHERE transfer_id = OLD.transfer_id
  ), 0) != CASE WHEN OLD.kind = 'fee_withdrawal' THEN 0
                ELSE OLD.amount_units + OLD.network_fee_units
                     + OLD.earned_fee_units END
  THEN RAISE(ABORT, 'confirmed transfer allocations do not balance') END;
  SELECT CASE WHEN EXISTS (
    SELECT 1
    FROM (
      SELECT credit_id, bucket, SUM(units) AS allocated_units
      FROM transfer_credit_allocations
      GROUP BY credit_id, bucket
    ) a
    JOIN deposit_credits c ON c.credit_id = a.credit_id
    WHERE a.allocated_units > CASE a.bucket
      WHEN 'main' THEN c.main_units ELSE c.recovery_units END
  ) THEN RAISE(ABORT, 'allocated credit capacity changed') END;
END;
CREATE TRIGGER IF NOT EXISTS transfer_confirmation_evidence_guard
BEFORE UPDATE ON transfers
BEGIN
  SELECT CASE WHEN OLD.state != 'confirmed' AND NEW.state != 'confirmed' AND (
      NEW.confirmed_at IS NOT OLD.confirmed_at OR
      NEW.confirmed_block_hash IS NOT OLD.confirmed_block_hash OR
      NEW.confirmed_block_height IS NOT OLD.confirmed_block_height OR
      NEW.confirmations != OLD.confirmations
    ) THEN RAISE(ABORT, 'confirmation evidence requires reconciliation transition') END;
  SELECT CASE WHEN OLD.state = 'confirmed' AND NEW.state = 'confirmed' AND (
      NEW.confirmed_at IS NOT OLD.confirmed_at OR
      NEW.confirmed_block_hash IS NOT OLD.confirmed_block_hash OR
      NEW.confirmed_block_height IS NOT OLD.confirmed_block_height OR
      NEW.confirmations < OLD.confirmations
    ) THEN RAISE(ABORT, 'confirmed anchor is immutable') END;
  SELECT CASE WHEN OLD.state = 'confirmed' AND NEW.state = 'uncertain' AND (
      NEW.confirmed_at IS NOT OLD.confirmed_at OR
      NEW.confirmed_block_hash IS NOT OLD.confirmed_block_hash OR
      NEW.confirmed_block_height IS NOT OLD.confirmed_block_height OR
      NEW.confirmations != OLD.confirmations
    ) THEN RAISE(ABORT, 'reorg transition must preserve prior evidence') END;
END;
CREATE TABLE IF NOT EXISTS audit_events (
  event_id INTEGER PRIMARY KEY AUTOINCREMENT,
  order_id INTEGER,
  actor_id INTEGER,
  event_type TEXT NOT NULL CHECK(
    instr(event_type, char(0)) = 0 AND
    length(CAST(event_type AS BLOB)) BETWEEN 1 AND 80 AND
    event_type NOT GLOB '*[^a-z0-9:_-]*'
  ),
  old_state TEXT CHECK(old_state IS NULL OR (
    instr(old_state, char(0)) = 0 AND
    length(CAST(old_state AS BLOB)) BETWEEN 1 AND 48 AND
    old_state NOT GLOB '*[^a-z0-9_]*'
  )),
  new_state TEXT CHECK(new_state IS NULL OR (
    instr(new_state, char(0)) = 0 AND
    length(CAST(new_state AS BLOB)) BETWEEN 1 AND 48 AND
    new_state NOT GLOB '*[^a-z0-9_]*'
  )),
  detail_json TEXT NOT NULL DEFAULT '{}' CHECK(
    instr(detail_json, char(0)) = 0 AND
    length(CAST(detail_json AS BLOB)) BETWEEN 2 AND 4000 AND
    json_valid(detail_json)
  ),
  created_at INTEGER NOT NULL
) STRICT;
CREATE TRIGGER IF NOT EXISTS audit_event_update_block
BEFORE UPDATE ON audit_events
BEGIN
  SELECT RAISE(ABORT, 'audit events are append-only');
END;
CREATE TRIGGER IF NOT EXISTS audit_event_delete_block
BEFORE DELETE ON audit_events
BEGIN
  SELECT RAISE(ABORT, 'audit events are append-only');
END;
INSERT INTO schema_meta(id, version) VALUES(1, 4)
ON CONFLICT(id) DO UPDATE SET version=excluded.version;
