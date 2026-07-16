import { invoke } from '@tauri-apps/api/core'

async function call<T>(command: string, payload?: Record<string, unknown>): Promise<T> {
  const raw = await invoke<string>(`plugin:wallet-core|${command}`, payload ? { payload } : {})
  return JSON.parse(raw) as T
}

export const status = <T>() => call<T>('status')
export const createWallet = <T>() => call<T>('create_wallet')
export const restoreWallet = <T>(recoveryPhrase: string) => call<T>('restore_wallet', { recoveryPhrase })
export const unlock = <T>() => call<T>('unlock')
export const lock = <T>() => call<T>('lock')
export const receive = <T>() => call<T>('receive')
export const activity = <T>(limit = 100) => call<T>('activity', { limit })
export const previewSend = <T>(destination: string, amount: string, fee: string) =>
  call<T>('preview_send', { destination, amount, fee })
export const confirmSend = <T>(pendingId: string) => call<T>('confirm_send', { pendingId })
export const cancelSend = <T>(pendingId: string) => call<T>('cancel_send', { pendingId })
export const recoveryPhrase = <T>() => call<T>('recovery_phrase')
