import Foundation
import Mobilewallet
import Tauri
import UIKit

final class RestoreArgs: Decodable { let recoveryPhrase: String }
final class ActivityArgs: Decodable { let limit: Int }
final class PreviewSendArgs: Decodable {
  let destination: String
  let amount: String
  let fee: String
}
final class PendingArgs: Decodable { let pendingId: String }

final class WalletCorePlugin: Plugin {
  private let deviceKeys = DeviceKeyStore()
  private let walletQueue = DispatchQueue(label: "org.bitcoin09.wallet-core", qos: .userInitiated)
  private lazy var engine: MobilewalletEngine? = makeEngine()
  private var privacyOverlay: UIView?

  override init() {
    super.init()
    NotificationCenter.default.addObserver(
      self,
      selector: #selector(protectForSnapshot),
      name: UIApplication.willResignActiveNotification,
      object: nil
    )
    NotificationCenter.default.addObserver(
      self,
      selector: #selector(lockForBackground),
      name: UIApplication.didEnterBackgroundNotification,
      object: nil
    )
    NotificationCenter.default.addObserver(
      self,
      selector: #selector(removePrivacyOverlay),
      name: UIApplication.didBecomeActiveNotification,
      object: nil
    )
    NotificationCenter.default.addObserver(
      self,
      selector: #selector(lockForBackground),
      name: UIApplication.protectedDataWillBecomeUnavailableNotification,
      object: nil
    )
  }

  deinit {
    engine?.close()
    privacyOverlay?.removeFromSuperview()
    NotificationCenter.default.removeObserver(self)
  }

  @objc private func lockForBackground() {
    walletQueue.async { self.engine?.lock() }
  }

  @objc private func protectForSnapshot() {
    lockForBackground()
    guard privacyOverlay == nil else { return }
    guard let window = UIApplication.shared.connectedScenes
      .compactMap({ $0 as? UIWindowScene })
      .flatMap({ $0.windows })
      .first(where: { $0.isKeyWindow }) else { return }

    let overlay = UIView(frame: window.bounds)
    overlay.autoresizingMask = [.flexibleWidth, .flexibleHeight]
    overlay.backgroundColor = UIColor(red: 23 / 255, green: 26 / 255, blue: 22 / 255, alpha: 1)
    overlay.accessibilityViewIsModal = true
    overlay.accessibilityLabel = "BTC09 Wallet locked"

    let label = UILabel()
    label.translatesAutoresizingMaskIntoConstraints = false
    label.numberOfLines = 2
    label.textAlignment = .center
    label.textColor = UIColor(red: 245 / 255, green: 241 / 255, blue: 232 / 255, alpha: 1)
    label.font = .systemFont(ofSize: 20, weight: .semibold)
    label.text = "BTC09 Wallet\nLocked"
    overlay.addSubview(label)
    NSLayoutConstraint.activate([
      label.centerXAnchor.constraint(equalTo: overlay.centerXAnchor),
      label.centerYAnchor.constraint(equalTo: overlay.centerYAnchor),
    ])

    window.addSubview(overlay)
    privacyOverlay = overlay
  }

  @objc private func removePrivacyOverlay() {
    privacyOverlay?.removeFromSuperview()
    privacyOverlay = nil
  }

  @objc func status(_ invoke: Invoke) throws {
    complete(invoke) { try self.engineCall { $0.status($1) } }
  }

  @objc func createWallet(_ invoke: Invoke) throws {
    withNewDeviceKey(invoke) { key in try self.engineCall { $0.createWallet(key, error: $1) } }
  }

  @objc func restoreWallet(_ invoke: Invoke) throws {
    let args = try invoke.parseArgs(RestoreArgs.self)
    withNewDeviceKey(invoke) { key in
      try self.engineCall { $0.restoreWallet(key, recoveryPhrase: args.recoveryPhrase, error: $1) }
    }
  }

  @objc func unlock(_ invoke: Invoke) throws {
    withDeviceKey(invoke, reason: "Open your BTC09 wallet") { key in
      try self.engineCall { $0.unlock(key, error: $1) }
    }
  }

  @objc func lock(_ invoke: Invoke) throws {
    complete(invoke) {
      self.engine?.lock()
      return "{}"
    }
  }

  @objc func receive(_ invoke: Invoke) throws {
    complete(invoke) { try self.engineCall { $0.receive($1) } }
  }

  @objc func activity(_ invoke: Invoke) throws {
    let args = try invoke.parseArgs(ActivityArgs.self)
    complete(invoke) { try self.engineCall { $0.activity(args.limit, error: $1) } }
  }

  @objc func previewSend(_ invoke: Invoke) throws {
    let args = try invoke.parseArgs(PreviewSendArgs.self)
    complete(invoke) {
      try self.engineCall {
        $0.previewSend(args.destination, amountText: args.amount, feeText: args.fee, error: $1)
      }
    }
  }

  @objc func confirmSend(_ invoke: Invoke) throws {
    let args = try invoke.parseArgs(PendingArgs.self)
    withDeviceKey(invoke, reason: "Confirm this BTC09 payment") { _ in
      try self.engineCall { $0.confirmSend(args.pendingId, error: $1) }
    }
  }

  @objc func cancelSend(_ invoke: Invoke) throws {
    let args = try invoke.parseArgs(PendingArgs.self)
    complete(invoke) {
      try self.engineVoidCall { try $0.cancelSend(args.pendingId) }
      return "{}"
    }
  }

  @objc func recoveryPhrase(_ invoke: Invoke) throws {
    withDeviceKey(invoke, reason: "Show your recovery words") { key in
      try self.engineCall { $0.recoveryPhrase(key, error: $1) }
    }
  }

  private func makeEngine() -> MobilewalletEngine? {
    do {
      var storage = try FileManager.default.url(
        for: .applicationSupportDirectory,
        in: .userDomainMask,
        appropriateFor: nil,
        create: true
      ).appendingPathComponent("wallet", isDirectory: true)
      try FileManager.default.createDirectory(at: storage, withIntermediateDirectories: true)
      var values = URLResourceValues()
      values.isExcludedFromBackup = true
      try storage.setResourceValues(values)
      try FileManager.default.setAttributes(
        [.protectionKey: FileProtectionType.complete],
        ofItemAtPath: storage.path
      )
      var error: NSError?
      let result = MobilewalletNewEngine(storage.path, "https://btc09.org", MobilewalletMainnet(), &error)
      return error == nil ? result : nil
    } catch {
      return nil
    }
  }

  private func withNewDeviceKey(_ invoke: Invoke, operation: @escaping (Data) throws -> String) {
    do {
      var key = try deviceKeys.createOrLoad()
      complete(invoke) {
        defer { key.resetBytes(in: 0..<key.count) }
        return try operation(key)
      }
    } catch {
      invoke.reject(publicMessage(error))
    }
  }

  private func withDeviceKey(_ invoke: Invoke, reason: String, operation: @escaping (Data) throws -> String) {
    do {
      var key = try deviceKeys.load(reason: reason)
      complete(invoke) {
        defer { key.resetBytes(in: 0..<key.count) }
        return try operation(key)
      }
    } catch {
      invoke.reject(publicMessage(error))
    }
  }

  private func complete(_ invoke: Invoke, operation: @escaping () throws -> String) {
    walletQueue.async {
      do {
        let data = try operation()
        DispatchQueue.main.async { invoke.resolve(["data": data]) }
      } catch {
        let message = self.publicMessage(error)
        DispatchQueue.main.async { invoke.reject(message) }
      }
    }
  }

  private func engineCall(
    _ operation: (MobilewalletEngine, NSErrorPointer) -> String?
  ) throws -> String {
    guard let engine else { throw WalletCoreError.message("BTC09 Wallet could not start safely.") }
    var error: NSError?
    let result = operation(engine, &error)
    if let error { throw error }
    guard let result else { throw WalletCoreError.message("BTC09 Wallet could not complete that.") }
    return result
  }

  private func engineVoidCall(
    _ operation: (MobilewalletEngine) throws -> Void
  ) throws {
    guard let engine else { throw WalletCoreError.message("BTC09 Wallet could not start safely.") }
    try operation(engine)
  }

  private func publicMessage(_ error: Error) -> String {
    if case let WalletCoreError.message(message) = error { return message }
    let message = (error as NSError).localizedDescription.trimmingCharacters(in: .whitespacesAndNewlines)
    let allowed = ["The ", "A ", "Check ", "Enter ", "Choose ", "Finish ", "Unlock "]
    return allowed.contains(where: { message.hasPrefix($0) })
      ? message
      : "BTC09 Wallet couldn't complete that. Try again."
  }
}

@_cdecl("init_plugin_wallet_core")
func initPlugin() -> Plugin { WalletCorePlugin() }
