const STATE_STORE = "state";
const ITEM_STORE = "items";
const BUNDLE_KEY = "pairing-bundle";
const SETTINGS_KEY = "settings";

function copy(value) {
  return value == null ? value : structuredClone(value);
}

export function createMemoryBackend() {
  const stores = new Map([[STATE_STORE, new Map()], [ITEM_STORE, new Map()]]);
  return {
    async get(store, key) { return copy(stores.get(store)?.get(key) ?? null); },
    async put(store, key, value) { stores.get(store).set(key, copy(value)); },
    async delete(store, key) { stores.get(store).delete(key); },
    async clear(store) { stores.get(store).clear(); },
    async values(store) { return [...stores.get(store).values()].map(copy); },
  };
}

export function createIndexedDBBackend(indexedDBFactory = globalThis.indexedDB) {
  if (!indexedDBFactory) throw new Error("IndexedDB is not available.");
  let databasePromise;
  const database = () => {
    if (!databasePromise) {
      databasePromise = new Promise((resolve, reject) => {
        const request = indexedDBFactory.open("nine-inbox", 1);
        request.onupgradeneeded = () => {
          const db = request.result;
          if (!db.objectStoreNames.contains(STATE_STORE)) db.createObjectStore(STATE_STORE);
          if (!db.objectStoreNames.contains(ITEM_STORE)) db.createObjectStore(ITEM_STORE, { keyPath: "id" });
        };
        request.onsuccess = () => resolve(request.result);
        request.onerror = () => reject(request.error || new Error("Could not open local inbox storage."));
        request.onblocked = () => reject(new Error("Local inbox storage is blocked by another tab."));
      });
    }
    return databasePromise;
  };
  const request = async (storeName, mode, operation) => {
    const db = await database();
    return new Promise((resolve, reject) => {
      const transaction = db.transaction(storeName, mode);
      const store = transaction.objectStore(storeName);
      let result;
      try { result = operation(store); } catch (error) { reject(error); return; }
      transaction.oncomplete = () => resolve(result?.result ?? null);
      transaction.onerror = () => reject(transaction.error || result?.error || new Error("Local inbox storage failed."));
      transaction.onabort = () => reject(transaction.error || new Error("Local inbox storage was cancelled."));
    });
  };
  return {
    get: (store, key) => request(store, "readonly", (objectStore) => objectStore.get(key)),
    put: (store, key, value) => request(store, "readwrite", (objectStore) => store === ITEM_STORE ? objectStore.put(copy(value)) : objectStore.put(copy(value), key)),
    delete: (store, key) => request(store, "readwrite", (objectStore) => objectStore.delete(key)),
    clear: (store) => request(store, "readwrite", (objectStore) => objectStore.clear()),
    values: (store) => request(store, "readonly", (objectStore) => objectStore.getAll()).then((value) => value || []),
  };
}

function validBundle(bundle) {
  return bundle && bundle.v === 1 && typeof bundle.apiBase === "string" && typeof bundle.mailboxId === "string" &&
    bundle.key instanceof Uint8Array && bundle.key.length === 32 &&
    bundle.writeToken instanceof Uint8Array && bundle.writeToken.length === 32 &&
    bundle.recoveryToken instanceof Uint8Array && bundle.recoveryToken.length === 16;
}

function validCachedItem(item) {
  return item && typeof item.id === "string" && item.id.length >= 20 && item.id.length <= 64 &&
    typeof item.createdAt === "string" && Number.isFinite(new Date(item.createdAt).getTime()) &&
    (!item.data || item.data instanceof Uint8Array);
}

function normalizeSettings(settings) {
  const value = settings || { deviceLabel: "This device", retention: "standard" };
  if (typeof value.deviceLabel !== "string" || value.deviceLabel.trim().length < 1 || value.deviceLabel.length > 64 ||
      !["standard", "pinned"].includes(value.retention)) {
    throw new Error("Invalid device settings.");
  }
  return { deviceLabel: value.deviceLabel.trim(), retention: value.retention };
}

export function createInboxStorage(backend = createIndexedDBBackend()) {
  if (!backend || !["get", "put", "delete", "clear", "values"].every((method) => typeof backend[method] === "function")) {
    throw new Error("Invalid local storage backend.");
  }
  return {
    async saveBundle(bundle) {
      if (!validBundle(bundle)) throw new Error("Invalid pairing bundle.");
      await backend.put(STATE_STORE, BUNDLE_KEY, copy(bundle));
    },
    async loadBundle() {
      const value = await backend.get(STATE_STORE, BUNDLE_KEY);
      if (value == null) return null;
      if (!validBundle(value)) throw new Error("Saved pairing data is invalid.");
      return copy(value);
    },
    async clearBundle() { await backend.delete(STATE_STORE, BUNDLE_KEY); },
    async putCachedItem(item) {
      if (!validCachedItem(item)) throw new Error("Invalid cached item.");
      await backend.put(ITEM_STORE, item.id, copy(item));
    },
    async getCachedItem(id) {
      const value = await backend.get(ITEM_STORE, id);
      return value == null ? null : copy(value);
    },
    async deleteCachedItem(id) { await backend.delete(ITEM_STORE, id); },
    async clearCachedItems() { await backend.clear(ITEM_STORE); },
    async listCachedItems() {
      const values = (await backend.values(ITEM_STORE)).filter(validCachedItem);
      values.sort((left, right) => right.createdAt.localeCompare(left.createdAt) || right.id.localeCompare(left.id));
      return values.map(copy);
    },
    async saveSettings(settings) { await backend.put(STATE_STORE, SETTINGS_KEY, normalizeSettings(settings)); },
    async loadSettings() {
      const value = await backend.get(STATE_STORE, SETTINGS_KEY);
      return normalizeSettings(value || undefined);
    },
  };
}
