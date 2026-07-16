package org.bitcoin09.walletcore

import android.content.Context
import android.os.Build
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.Base64
import java.security.KeyStore
import java.security.SecureRandom
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

internal class DeviceKeyStore(private val context: Context) {
    private val preferences = context.getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE)

    @Synchronized
    fun createOrLoad(): ByteArray {
        val hasCiphertext = preferences.contains(CIPHERTEXT)
        val hasIV = preferences.contains(IV)
        if (hasCiphertext != hasIV) {
            throw IllegalStateException("This device no longer has the complete key for this wallet.")
        }
        if (hasCiphertext) {
            return load()
        }
        val deviceKey = ByteArray(DEVICE_KEY_BYTES)
        try {
            SecureRandom().nextBytes(deviceKey)
            val cipher = Cipher.getInstance(TRANSFORMATION)
            cipher.init(Cipher.ENCRYPT_MODE, wrappingKey())
            val ciphertext = cipher.doFinal(deviceKey)
            val saved = try {
                preferences.edit()
                    .putString(CIPHERTEXT, Base64.encodeToString(ciphertext, Base64.NO_WRAP))
                    .putString(IV, Base64.encodeToString(cipher.iv, Base64.NO_WRAP))
                    .commit()
            } finally {
                ciphertext.fill(0)
            }
            if (!saved) {
                throw IllegalStateException("The device security key could not be saved.")
            }
            return deviceKey
        } catch (error: Exception) {
            deviceKey.fill(0)
            throw error
        }
    }

    @Synchronized
    fun load(): ByteArray {
        val ciphertext = preferences.getString(CIPHERTEXT, null)
            ?.let { Base64.decode(it, Base64.NO_WRAP) }
            ?: throw IllegalStateException("This device no longer has the key for this wallet.")
        val iv = preferences.getString(IV, null)
            ?.let { Base64.decode(it, Base64.NO_WRAP) }
            ?: throw IllegalStateException("This device no longer has the key for this wallet.")
        return try {
            val cipher = Cipher.getInstance(TRANSFORMATION)
            cipher.init(Cipher.DECRYPT_MODE, wrappingKey(), GCMParameterSpec(128, iv))
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

    private fun wrappingKey(): SecretKey {
        val keyStore = KeyStore.getInstance(KEYSTORE).apply { load(null) }
        (keyStore.getKey(KEY_ALIAS, null) as? SecretKey)?.let { return it }
        val generator = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, KEYSTORE)
        val builder = KeyGenParameterSpec.Builder(
            KEY_ALIAS,
            KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT,
        )
            .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
            .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
            .setKeySize(256)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
            builder.setUnlockedDeviceRequired(true)
        }
        generator.init(builder.build())
        return generator.generateKey()
    }

    private companion object {
        const val DEVICE_KEY_BYTES = 32
        const val PREFERENCES = "btc09_device_key"
        const val CIPHERTEXT = "ciphertext"
        const val IV = "iv"
        const val KEYSTORE = "AndroidKeyStore"
        const val KEY_ALIAS = "org.bitcoin09.wallet.device-key"
        const val TRANSFORMATION = "AES/GCM/NoPadding"
    }
}
