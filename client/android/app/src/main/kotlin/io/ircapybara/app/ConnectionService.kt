package io.ircapybara.app

import android.app.Notification
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.graphics.drawable.Icon
import android.os.Build
import android.os.IBinder

/**
 * Foreground service that exists only while a server session does. It carries no
 * logic of its own; the connection lives in the Go core. Its job is to keep the
 * process from being frozen or killed while the app is in the background.
 */
class ConnectionService : Service() {

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        val app = application as IRCapybaraApp
        if (intent?.action == ACTION_DISCONNECT) {
            app.calls.execute { app.bridge.disconnect() }
            stopForeground(STOP_FOREGROUND_REMOVE)
            stopSelf()
            return START_NOT_STICKY
        }

        val notification = buildNotification(this, intent?.getStringExtra(EXTRA_LABEL).orEmpty())
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            startForeground(NOTIFICATION_ID, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE)
        } else {
            startForeground(NOTIFICATION_ID, notification)
        }
        // Not sticky: after process death there is no session to resume, because
        // passwords are deliberately never stored.
        return START_NOT_STICKY
    }

    /**
     * Swiping the app out of recents removes the web view that decides which
     * messages notify. Staying connected without it would silently swallow
     * messages, so end the session instead.
     */
    override fun onTaskRemoved(rootIntent: Intent?) {
        val app = application as IRCapybaraApp
        app.calls.execute { app.bridge.disconnect() }
        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelf()
    }

    companion object {
        private const val NOTIFICATION_ID = 1
        private const val EXTRA_LABEL = "label"
        private const val ACTION_DISCONNECT = "io.ircapybara.app.DISCONNECT"

        @Volatile
        private var running = false

        /** Called by the host whenever the core reports that a session started, changed or ended. */
        fun update(context: Context, active: Boolean, label: String) {
            val wasRunning = running
            running = active
            if (!active) {
                context.stopService(Intent(context, ConnectionService::class.java))
                return
            }
            if (wasRunning) {
                // Starting a foreground service again is not allowed from the
                // background; refreshing its notification is.
                context.getSystemService(NotificationManager::class.java)
                    .notify(NOTIFICATION_ID, buildNotification(context, label))
                return
            }
            try {
                context.startForegroundService(
                    Intent(context, ConnectionService::class.java).putExtra(EXTRA_LABEL, label),
                )
            } catch (_: RuntimeException) {
                // The session was started while the app was not in the foreground.
                // It keeps working while the app is visible.
                running = false
            }
        }

        private fun buildNotification(context: Context, label: String): Notification {
            val open = PendingIntent.getActivity(
                context,
                0,
                Intent(context, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
                PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
            )
            val disconnect = PendingIntent.getService(
                context,
                1,
                Intent(context, ConnectionService::class.java).setAction(ACTION_DISCONNECT),
                PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
            )
            val title = if (label.isBlank()) {
                context.getString(R.string.connection_title_generic)
            } else {
                context.getString(R.string.connection_title, label)
            }
            return Notification.Builder(context, IRCapybaraApp.CHANNEL_CONNECTION)
                .setSmallIcon(R.drawable.ic_notification)
                .setContentTitle(title)
                .setContentText(context.getString(R.string.connection_text))
                .setContentIntent(open)
                .setOngoing(true)
                .setCategory(Notification.CATEGORY_SERVICE)
                .addAction(
                    Notification.Action.Builder(null as Icon?, context.getString(R.string.connection_disconnect), disconnect).build(),
                )
                .build()
        }
    }
}
