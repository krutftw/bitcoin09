package org.bitcoin09.walletcore

import android.content.Context
import android.os.Build
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyInfo
import android.security.keystore.KeyProperties
import android.util.Base64
import java.security.KeyStore
import java.security.SecureRandom
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.SecretKeyFactory
import javax.crypto.spec.GCMParameterSpec

internal class DeviceKeyStore(private val context: Context) {
    private val preferences = context.getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE)

    @Synchronized
    fun createOrLoad(): ByteArray {
        val hasCiphertext = preferences.contains(CIPHERTEXT_V2)
        val hasIV = preferences.contains(IV_V2)
        if (hasCiphertext != hasIV) {
            throw IllegalStateException("This device no longer has the complete key for this wallet.")
        }
        if (hasCiphertext) {
            return load()
        }
        val hasLegacyCiphertext = preferences.contains(LEGACY_CIPHERTEXT)
        val hasLegacyIV = preferences.contains(LEGACY_IV)
        if (hasLegacyCiphertext != hasLegacyIV) {
            throw IllegalStateException("This device no longer has the complete key for this wallet.")
        }
        if (hasLegacyCiphertext) {
            return migrateLegacyKey()
        }
        val deviceKey = ByteArray(DEVICE_KEY_BYTES)
        try {
            SecureRandom().nextBytes(deviceKey)
            saveCurrent(deviceKey)
            return deviceKey
        } catch (error: Exception) {
            deviceKey.fill(0)
            throw error
        }
    }

    @Synchronized
    fun load(): ByteArray {
        val hasCiphertext = preferences.contains(CIPHERTEXT_V2)
        val hasIV = preferences.contains(IV_V2)
        if (hasCiphertext != hasIV) {
            throw IllegalStateException("This device no longer has the complete key for this wallet.")
        }
        if (!hasCiphertext) {
            val hasLegacyCiphertext = preferences.contains(LEGACY_CIPHERTEXT)
            val hasLegacyIV = preferences.contains(LEGACY_IV)
            if (hasLegacyCiphertext != hasLegacyIV) {
                throw IllegalStateException("This device no longer has the complete key for this wallet.")
            }
            if (hasLegacyCiphertext) {
                return migrateLegacyKey()
            }
            throw IllegalStateException("This device no longer has the key for this wallet.")
        }
        return decrypt(CIPHERTEXT_V2, IV_V2, authenticationBoundWrappingKey())
    }

    private fun migrateLegacyKey(): ByteArray {
        val keyStore = KeyStore.getInstance(KEYSTORE).apply { load(null) }
        val legacyWrappingKey = keyStore.getKey(LEGACY_KEY_ALIAS, null) as? SecretKey
            ?: throw IllegalStateException("This device no longer has the key for this wallet.")
        val deviceKey = decrypt(LEGACY_CIPHERTEXT, LEGACY_IV, legacyWrappingKey)
        try {
            saveCurrent(deviceKey)
            preferences.edit()
                .remove(LEGACY_CIPHERTEXT)
                .remove(LEGACY_IV)
                .commit()
            keyStore.deleteEntry(LEGACY_KEY_ALIAS)
            return deviceKey
        } catch (error: Exception) {
            deviceKey.fill(0)
            throw error
        }
    }

    private fun saveCurrent(deviceKey: ByteArray) {
        val cipher = Cipher.getInstance(TRANSFORMATION)
        cipher.init(Cipher.ENCRYPT_MODE, authenticationBoundWrappingKey())
        val ciphertext = cipher.doFinal(deviceKey)
        val saved = try {
            preferences.edit()
                .putString(CIPHERTEXT_V2, Base64.encodeToString(ciphertext, Base64.NO_WRAP))
                .putString(IV_V2, Base64.encodeToString(cipher.iv, Base64.NO_WRAP))
                .commit()
        } finally {
            ciphertext.fill(0)
        }
        if (!saved) {
            throw IllegalStateException("The device security key could not be saved.")
        }
    }

    private fun decrypt(ciphertextName: String, ivName: String, wrappingKey: SecretKey): ByteArray {
        val ciphertext = preferences.getString(ciphertextName, null)
            ?.let { Base64.decode(it, Base64.NO_WRAP) }
            ?: throw IllegalStateException("This device no longer has the key for this wallet.")
        val iv = preferences.getString(ivName, null)
            ?.let { Base64.decode(it, Base64.NO_WRAP) }
            ?: throw IllegalStateException("This device no longer has the key for this wallet.")
        return try {
            val cipher = Cipher.getInstance(TRANSFORMATION)
            cipher.init(Cipher.DECRYPT_MODE, wrappingKey, GCMParameterSpec(128, iv))
            val deviceKey = cipher.doFinal(ciphertext)
            if (deviceKey.size != DEVICE_KEY_BYTES) {
                deviceKey.fill(0)
                throw IllegalStateException("The device security key is not valid.")
            }
            deviceKey
        } finally {
            ciphertext.fill(0)
            iv.fill(0)
        }
    }

    private fun authenticationBoundWrappingKey(): SecretKey {
        val keyStore = KeyStore.getInstance(KEYSTORE).apply { load(null) }
        (keyStore.getKey(KEY_ALIAS_V2, null) as? SecretKey)?.let { key ->
            val factory = SecretKeyFactory.getInstance(key.algorithm, KEYSTORE)
            val info = factory.getKeySpec(key, KeyInfo::class.java) as KeyInfo
            if (!info.isUserAuthenticationRequired) {
                throw IllegalStateException("The device security key is not protected correctly.")
            }
            return key
        }
        val generator = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, KEYSTORE)
        val builder = KeyGenParameterSpec.Builder(
            KEY_ALIAS_V2,
            KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT,
        )
            .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
            .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
            .setKeySize(256)
            .setUserAuthenticationRequired(true)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            builder.setUserAuthenticationParameters(
                AUTHENTICATION_WINDOW_SECONDS,
                KeyProperties.AUTH_BIOMETRIC_STRONG or KeyProperties.AUTH_DEVICE_CREDENTIAL,
            )
        } else {
            @Suppress("DEPRECATION")
            builder.setUserAuthenticationValidityDurationSeconds(AUTHENTICATION_WINDOW_SECONDS)
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
            builder.setUnlockedDeviceRequired(true)
        }
        generator.init(builder.build())
        return generator.generateKey()
    }

    private companion object {
        const val DEVICE_KEY_BYTES = 32
        const val AUTHENTICATION_WINDOW_SECONDS = 15
        const val PREFERENCES = "btc09_device_key"
        const val CIPHERTEXT_V2 = "ciphertext_v2"
        const val IV_V2 = "iv_v2"
        const val LEGACY_CIPHERTEXT = "ciphertext"
        const val LEGACY_IV = "iv"
        const val KEYSTORE = "AndroidKeyStore"
        const val KEY_ALIAS_V2 = "org.bitcoin09.wallet.device-key.v2"
        const val LEGACY_KEY_ALIAS = "org.bitcoin09.wallet.device-key"
        const val TRANSFORMATION = "AES/GCM/NoPadding"
    }
}
