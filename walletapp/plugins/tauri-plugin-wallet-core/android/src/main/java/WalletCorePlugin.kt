package org.bitcoin09.walletcore

import android.app.Activity
import android.app.KeyguardManager
import android.os.Build
import androidx.biometric.BiometricManager
import androidx.biometric.BiometricPrompt
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import androidx.fragment.app.FragmentActivity
import app.tauri.annotation.Command
import app.tauri.annotation.InvokeArg
import app.tauri.annotation.TauriPlugin
import app.tauri.plugin.Invoke
import app.tauri.plugin.JSObject
import app.tauri.plugin.Plugin
import org.bitcoin09.mobilewallet.Engine
import org.bitcoin09.mobilewallet.Mobilewallet
import java.io.File
import java.util.concurrent.ExecutorService
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicBoolean

@InvokeArg
class RestoreArgs {
    lateinit var recoveryPhrase: String
}

@InvokeArg
class ActivityArgs {
    var limit: Long = 100
}

@InvokeArg
class PreviewSendArgs {
    lateinit var destination: String
    lateinit var amount: String
    lateinit var fee: String
}

@InvokeArg
class PendingArgs {
    lateinit var pendingId: String
}

@TauriPlugin
class WalletCorePlugin(private val activity: Activity) : Plugin(activity) {
    private val deviceKeys = DeviceKeyStore(activity.applicationContext)
    private val worker: ExecutorService = Executors.newSingleThreadExecutor()
    private val authenticationInProgress = AtomicBoolean(false)
    @Volatile private var activePrompt: BiometricPrompt? = null
    @Volatile private var engineInstance: Engine? = null

    private fun engine(): Engine {
        engineInstance?.let { return it }
        return synchronized(this) {
            engineInstance ?: run {
                val storage = File(activity.filesDir, "wallet").absolutePath
                Mobilewallet.newEngine(storage, WALLET_GATEWAY, Mobilewallet.mainnet())
                    .also { engineInstance = it }
            }
        }
    }

    override fun onPause() {
        // A device-credential fallback can pause the activity while Android owns
        // the security screen. Its callback will either continue the operation or
        // lock the wallet on error, so do not cancel that system flow here.
        if (!authenticationInProgress.get()) {
            submit { engineInstance?.lock() }
        }
    }

    override fun onDestroy(activity: AppCompatActivity) {
        activePrompt?.cancelAuthentication()
        activePrompt = null
        authenticationInProgress.set(false)
        submit { engineInstance?.close() }
        worker.shutdown()
    }

    @Command
    fun status(invoke: Invoke) = complete(invoke) { engine().status() }

    @Command
    fun createWallet(invoke: Invoke) = withNewDeviceKey(invoke) { key -> engine().createWallet(key) }

    @Command
    fun restoreWallet(invoke: Invoke) {
        val args = invoke.parseArgs(RestoreArgs::class.java)
        withNewDeviceKey(invoke) { key -> engine().restoreWallet(key, args.recoveryPhrase) }
    }

    @Command
    fun unlock(invoke: Invoke) = withAuthentication(invoke, "Open your BTC09 wallet") { key ->
        engine().unlock(key)
    }

    @Command
    fun lock(invoke: Invoke) = complete(invoke) {
        engine().lock()
        "{}"
    }

    @Command
    fun receive(invoke: Invoke) = complete(invoke) { engine().receive() }

    @Command
    fun activity(invoke: Invoke) {
        val args = invoke.parseArgs(ActivityArgs::class.java)
        complete(invoke) { engine().activity(args.limit) }
    }

    @Command
    fun previewSend(invoke: Invoke) {
        val args = invoke.parseArgs(PreviewSendArgs::class.java)
        complete(invoke) { engine().previewSend(args.destination, args.amount, args.fee) }
    }

    @Command
    fun confirmSend(invoke: Invoke) {
        val args = invoke.parseArgs(PendingArgs::class.java)
        withAuthentication(invoke, "Confirm this BTC09 payment") { engine().confirmSend(args.pendingId) }
    }

    @Command
    fun cancelSend(invoke: Invoke) {
        val args = invoke.parseArgs(PendingArgs::class.java)
        complete(invoke) {
            engine().cancelSend(args.pendingId)
            "{}"
        }
    }

    @Command
    fun recoveryPhrase(invoke: Invoke) = withAuthentication(invoke, "Show your recovery words") { key ->
        engine().recoveryPhrase(key)
    }

    private fun withNewDeviceKey(invoke: Invoke, operation: (ByteArray) -> String) {
        authenticate(invoke, "Protect your new BTC09 wallet") {
            complete(invoke) {
                val key = deviceKeys.createOrLoad()
                try {
                    operation(key)
                } finally {
                    key.fill(0)
                }
            }
        }
    }

    private fun withAuthentication(invoke: Invoke, reason: String, operation: (ByteArray) -> String) {
        authenticate(invoke, reason) {
            complete(invoke) {
                val key = deviceKeys.load()
                try {
                    operation(key)
                } finally {
                    key.fill(0)
                }
            }
        }
    }

    private fun authenticate(invoke: Invoke, reason: String, authenticated: () -> Unit) {
        val owner = activity as? FragmentActivity
        if (owner == null) {
            invoke.reject("The device security prompt is not available.")
            return
        }
        if (!authenticationInProgress.compareAndSet(false, true)) {
            invoke.reject("Finish the device security check already on screen.")
            return
        }
        activity.runOnUiThread {
            val promptBuilder = BiometricPrompt.PromptInfo.Builder()
                .setTitle("BTC09 Wallet")
                .setSubtitle(reason)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                val allowed = BiometricManager.Authenticators.BIOMETRIC_STRONG or
                    BiometricManager.Authenticators.DEVICE_CREDENTIAL
                val availability = BiometricManager.from(activity).canAuthenticate(allowed)
                if (availability != BiometricManager.BIOMETRIC_SUCCESS) {
                    authenticationInProgress.set(false)
                    invoke.reject("Set up a screen lock, fingerprint, or face unlock before opening this wallet.")
                    return@runOnUiThread
                }
                promptBuilder.setAllowedAuthenticators(allowed)
            } else {
                val keyguard = activity.getSystemService(KeyguardManager::class.java)
                if (keyguard?.isDeviceSecure != true) {
                    authenticationInProgress.set(false)
                    invoke.reject("Set up a screen lock, fingerprint, or face unlock before opening this wallet.")
                    return@runOnUiThread
                }
                @Suppress("DEPRECATION")
                promptBuilder.setDeviceCredentialAllowed(true)
            }
            val prompt = BiometricPrompt(owner, ContextCompat.getMainExecutor(activity),
                object : BiometricPrompt.AuthenticationCallback() {
                    override fun onAuthenticationSucceeded(result: BiometricPrompt.AuthenticationResult) {
                        activePrompt = null
                        authenticationInProgress.set(false)
                        authenticated()
                    }

                    override fun onAuthenticationError(errorCode: Int, errString: CharSequence) {
                        activePrompt = null
                        authenticationInProgress.set(false)
                        submit { engineInstance?.lock() }
                        invoke.reject("The wallet stayed locked.")
                    }
                })
            val promptInfo = try {
                promptBuilder.build()
            } catch (_: IllegalArgumentException) {
                authenticationInProgress.set(false)
                invoke.reject("Set up a screen lock, fingerprint, or face unlock before opening this wallet.")
                return@runOnUiThread
            }
            activePrompt = prompt
            prompt.authenticate(promptInfo)
        }
    }

    private fun complete(invoke: Invoke, operation: () -> String) {
        submit {
            try {
                val response = JSObject()
                response.put("data", operation())
                invoke.resolve(response)
            } catch (error: Exception) {
                invoke.reject(publicMessage(error))
            }
        }
    }

    private fun submit(operation: () -> Unit) {
        try {
            worker.execute(operation)
        } catch (_: RuntimeException) {
            // The app is already closing; no wallet work should start now.
        }
    }

    private fun publicMessage(error: Exception): String {
        val message = error.message?.trim().orEmpty()
        return if (message.startsWith("The ") || message.startsWith("A ") ||
            message.startsWith("Check ") || message.startsWith("Enter ") ||
            message.startsWith("Choose ") || message.startsWith("Finish ") ||
            message.startsWith("Unlock ")) message
        else "BTC09 Wallet couldn't complete that. Try again."
    }

    private companion object {
        const val WALLET_GATEWAY = "https://btc09.org"
    }
}
