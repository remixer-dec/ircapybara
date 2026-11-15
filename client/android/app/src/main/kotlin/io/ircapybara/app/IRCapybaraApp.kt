package io.ircapybara.app

import android.app.Application
import android.app.NotificationChannel
import android.app.NotificationManager
import android.net.ConnectivityManager
import android.net.Network
import io.ircapybara.mobile.Bridge
import io.ircapybara.mobile.Mobile
import java.io.File
import java.util.concurrent.ExecutorService
import java.util.concurrent.Executors

/**
 * Owns the Go client core for the lifetime of the process.
 *
 * The core must outlive the activity: Android may destroy and recreate the
 * activity (and its web view) while the foreground service keeps the process,
 * and therefore the IRC connection, alive. A recreated page simply calls
 * Bootstrap again and receives the current session.
 */
class IRCapybaraApp : Application() {
    lateinit var host: AndroidHost
        private set
    lateinit var bridge: Bridge
        private set

    /** Calls into Go may block (file pickers, permission prompts), so they never run on the main thread. */
    val calls: ExecutorService = Executors.newCachedThreadPool()

    override fun onCreate() {
        super.onCreate()
        createNotificationChannels()

        // Upload copies only need to live as long as one transfer.
        File(cacheDir, AndroidHost.UPLOAD_DIR).deleteRecursively()

        host = AndroidHost(this)
        bridge = Mobile.start(
            host,
            File(filesDir, "config").absolutePath,
            File(filesDir, "downloads").absolutePath,
        )
        watchNetwork()
    }

    private fun createNotificationChannels() {
        val manager = getSystemService(NotificationManager::class.java)
        manager.createNotificationChannel(
            NotificationChannel(CHANNEL_CONNECTION, getString(R.string.channel_connection), NotificationManager.IMPORTANCE_LOW).apply {
                description = getString(R.string.channel_connection_description)
                setShowBadge(false)
            },
        )
        manager.createNotificationChannel(
            NotificationChannel(CHANNEL_MESSAGES, getString(R.string.channel_messages), NotificationManager.IMPORTANCE_HIGH),
        )
        manager.createNotificationChannel(
            NotificationChannel(CHANNEL_MESSAGES_QUIET, getString(R.string.channel_messages_quiet), NotificationManager.IMPORTANCE_LOW),
        )
    }

    /**
     * A TCP connection does not notice that the phone moved from Wi-Fi to mobile
     * data. Ask the core to probe the server whenever the default network
     * changes so a dead session becomes a visible disconnect quickly.
     */
    private fun watchNetwork() {
        val connectivity = getSystemService(ConnectivityManager::class.java) ?: return
        try {
            connectivity.registerDefaultNetworkCallback(object : ConnectivityManager.NetworkCallback() {
                override fun onAvailable(network: Network) = probe()
                override fun onLost(network: Network) = probe()
            })
        } catch (_: RuntimeException) {
            // Without the callback the session still ends on the next failed write.
        }
    }

    fun probe() {
        calls.execute { bridge.checkConnection() }
    }

    companion object {
        const val CHANNEL_CONNECTION = "connection"
        const val CHANNEL_MESSAGES = "messages"
        const val CHANNEL_MESSAGES_QUIET = "messages-quiet"
    }
}
