import Foundation
import LocalAuthentication
import Security

final class DeviceKeyStore {
  private let service = "org.bitcoin09.wallet.device-key"
  private let account = "primary"

  func createOrLoad() throws -> Data {
    if exists() {
      return try load(reason: "Open your BTC09 wallet")
    }
    var key = Data(count: 32)
    let status = key.withUnsafeMutableBytes { bytes in
      SecRandomCopyBytes(kSecRandomDefault, 32, bytes.baseAddress!)
    }
    guard status == errSecSuccess else {
      throw WalletCoreError.message("The device security key could not be created.")
    }
    var accessError: Unmanaged<CFError>?
    guard let access = SecAccessControlCreateWithFlags(
      kCFAllocatorDefault,
      kSecAttrAccessibleWhenUnlockedThisDeviceOnly,
      [.userPresence],
      &accessError
    ) else {
      key.resetBytes(in: 0..<key.count)
      throw WalletCoreError.message("The device security key could not be protected.")
    }
    let query: [String: Any] = [
      kSecClass as String: kSecClassGenericPassword,
      kSecAttrService as String: service,
      kSecAttrAccount as String: account,
      kSecAttrAccessControl as String: access,
      kSecAttrSynchronizable as String: false,
      kSecValueData as String: key,
    ]
    let addStatus = SecItemAdd(query as CFDictionary, nil)
    if addStatus == errSecDuplicateItem {
      key.resetBytes(in: 0..<key.count)
      return try load(reason: "Open your BTC09 wallet")
    }
    guard addStatus == errSecSuccess else {
      key.resetBytes(in: 0..<key.count)
      throw WalletCoreError.message("The device security key could not be saved.")
    }
    return key
  }

  func load(reason: String) throws -> Data {
    let context = LAContext()
    context.localizedCancelTitle = "Cancel"
    let query: [String: Any] = [
      kSecClass as String: kSecClassGenericPassword,
      kSecAttrService as String: service,
      kSecAttrAccount as String: account,
      kSecAttrSynchronizable as String: false,
      kSecReturnData as String: true,
      kSecMatchLimit as String: kSecMatchLimitOne,
      kSecUseAuthenticationContext as String: context,
      kSecUseOperationPrompt as String: reason,
    ]
    var value: CFTypeRef?
    let status = SecItemCopyMatching(query as CFDictionary, &value)
    guard status == errSecSuccess, let key = value as? Data, key.count == 32 else {
      throw WalletCoreError.message("The wallet stayed locked.")
    }
    return key
  }

  private func exists() -> Bool {
    let query: [String: Any] = [
      kSecClass as String: kSecClassGenericPassword,
      kSecAttrService as String: service,
      kSecAttrAccount as String: account,
      kSecAttrSynchronizable as String: false,
      kSecReturnAttributes as String: true,
      kSecUseAuthenticationUI as String: kSecUseAuthenticationUIFail,
    ]
    let status = SecItemCopyMatching(query as CFDictionary, nil)
    return status == errSecSuccess || status == errSecInteractionNotAllowed
  }
}

enum WalletCoreError: Error {
  case message(String)
}
