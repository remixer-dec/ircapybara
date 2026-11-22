package io.ircapybara.app

import android.Manifest
import android.app.Notification
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.ActivityNotFoundException
import android.content.ContentValues
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.Environment
import android.os.Handler
import android.os.Looper
import android.provider.DocumentsContract
import android.provider.MediaStore
import android.provider.OpenableColumns
import io.ircapybara.mobile.Host
import org.json.JSONObject
import java.io.File
import java.io.IOException
import java.util.Collections

/**
 * The platform half of the Go client core: everything the core cannot do itself
 * on Android. Each method corresponds to one method of the Go `mobile.Host`
 * interface.
 *
 * Go calls these from its own threads, never from the main thread, so the
 * methods that need the user (pickers, the notification permission) block until
 * the activity delivers an answer.
 */
class AndroidHost(private val app: IRCapybaraApp) : Host {
    private val main = Handler(Looper.getMainLooper())
    private val notifications = app.getSystemService(NotificationManager::class.java)
    private val messageNotificationIds: MutableSet<Int> = Collections.synchronizedSet(HashSet())

    /** The activity currently showing the web view, if any. */
    @Volatile
    var activity: MainActivity? = null

    override fun emit(event: String, payloadJSON: String) {
        // With no activity there is no page to update. The next page to load
        // asks the core for the full state, so nothing is lost.
        main.post { activity?.dispatchEvent(event, payloadJSON) }
    }

    override fun connectionChanged(active: Boolean, label: String) {
        main.post { ConnectionService.update(app, active, label) }
    }

    override fun chooseUpload(): String {
        val uri = requireActivity().pickDocument() ?: return ""
        val (name, mime) = describe(uri)
        val directory = File(app.cacheDir, UPLOAD_DIR).apply { mkdirs() }
        // The core reads uploads with ordinary file APIs, including re-reads for
        // retransmission, so the document is copied out of its content URI.
        val copy = File.createTempFile("upload-", ".bin", directory)
        try {
            copyBounded(uri, copy, MAX_UPLOAD_BYTES)
        } catch (e: Exception) {
            copy.delete()
            throw e
        }
        return JSONObject().put("path", copy.absolutePath).put("name", name).put("mime", mime).toString()
    }

    override fun publishDownload(path: String, name: String, mime: String): String {
        // On older Android versions the Open action exports the file through the system picker.
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.Q) return path
        val source = File(path)
        val resolver = app.contentResolver
        val values = ContentValues().apply {
            put(MediaStore.MediaColumns.DISPLAY_NAME, name)
            put(MediaStore.MediaColumns.MIME_TYPE, mime.ifBlank { "application/octet-stream" })
            put(MediaStore.MediaColumns.RELATIVE_PATH, Environment.DIRECTORY_DOWNLOADS + "/IRCapybara")
            put(MediaStore.MediaColumns.IS_PENDING, 1)
        }
        val collection = MediaStore.Downloads.getContentUri(MediaStore.VOLUME_EXTERNAL_PRIMARY)
        val uri = resolver.insert(collection, values) ?: throw IOException("could not create the download entry")
        try {
            val output = resolver.openOutputStream(uri) ?: throw IOException("could not open the download entry")
            output.use { out -> source.inputStream().use { it.copyTo(out) } }
            resolver.update(uri, ContentValues().apply { put(MediaStore.MediaColumns.IS_PENDING, 0) }, null, null)
        } catch (e: Exception) {
            resolver.delete(uri, null, null)
            throw e
        }
        source.delete()
        source.parentFile?.delete()
        return uri.toString()
    }

    override fun fileExists(location: String): Boolean {
        return try {
            val uri = Uri.parse(location)
            if (uri.scheme != "content") return File(location).isFile
            app.contentResolver.query(uri, arrayOf(MediaStore.MediaColumns._ID), null, null, null)
                ?.use { it.moveToFirst() } ?: false
        } catch (_: Exception) {
            false
        }
    }

    override fun openFile(location: String, mime: String) {
        val current = requireActivity()
        val parsed = Uri.parse(location)
        val uri = when (parsed.scheme) {
            "content" -> parsed
            null -> saveDownload(current, File(location), mime) ?: return
            else -> throw IOException("this download could not be opened")
        }
        val type = app.contentResolver.getType(uri) ?: mime.ifBlank { "*/*" }
        val view = Intent(Intent.ACTION_VIEW)
            .setDataAndType(uri, type)
            .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
        current.runOnUiThread {
            try {
                // Always a chooser: the file came from the network, so the user
                // decides what handles it.
                current.startActivity(Intent.createChooser(view, current.getString(R.string.open_with)))
            } catch (_: ActivityNotFoundException) {
                // Nothing can open this type; the saved file is still available.
            }
        }
    }

    private fun saveDownload(current: MainActivity, source: File, mime: String): Uri? {
        val intent = Intent(Intent.ACTION_CREATE_DOCUMENT)
            .addCategory(Intent.CATEGORY_OPENABLE)
            .setType(mime.ifBlank { "application/octet-stream" })
            .putExtra(Intent.EXTRA_TITLE, source.name)
        val uri = current.pickDocument(intent) ?: return null
        if (uri.scheme != "content") throw IOException("invalid save location")
        val resolver = app.contentResolver
        try {
            val output = resolver.openOutputStream(uri) ?: throw IOException("could not create the saved file")
            output.use { out -> source.inputStream().use { it.copyTo(out) } }
        } catch (e: Exception) {
            runCatching { DocumentsContract.deleteDocument(resolver, uri) }
            throw e
        }
        return uri
    }

    override fun chooseCertificate(): String {
        val uri = requireActivity().pickDocument() ?: return ""
        val (name, _) = describe(uri)
        val directory = File(app.filesDir, "pins").apply { mkdirs() }
        val target = File(directory, name.replace(Regex("[^A-Za-z0-9._-]"), "_").ifBlank { "certificate" })
        copyBounded(uri, target, MAX_CERTIFICATE_BYTES)
        return target.absolutePath
    }

    override fun prepareNotifications(): Boolean {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) return notifications.areNotificationsEnabled()
        if (app.checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED) {
            return notifications.areNotificationsEnabled()
        }
        val current = activity ?: return false
        return current.requestNotificationPermission()
    }

    @Suppress("DEPRECATION")
    override fun showNotification(title: String, subtitle: String, body: String, sound: Boolean) {
        if (!notifications.areNotificationsEnabled()) throw IOException("notifications are turned off for IRCapybara")
        val open = PendingIntent.getActivity(
            app,
            0,
            Intent(app, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val builder = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val channel = if (sound) IRCapybaraApp.CHANNEL_MESSAGES else IRCapybaraApp.CHANNEL_MESSAGES_QUIET
            Notification.Builder(app, channel)
        } else {
            Notification.Builder(app)
                .setPriority(if (sound) Notification.PRIORITY_HIGH else Notification.PRIORITY_LOW)
                .setDefaults(if (sound) Notification.DEFAULT_SOUND else 0)
        }
        val notification = builder
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(title)
            .setContentText(body)
            .setSubText(subtitle.ifBlank { null })
            .setStyle(Notification.BigTextStyle().bigText(body))
            .setCategory(Notification.CATEGORY_MESSAGE)
            .setContentIntent(open)
            .setAutoCancel(true)
            .build()
        // One notification per sender and conversation; a newer message replaces
        // the older one. IDs below 16 are reserved for the service.
        val id = 16 + ((title + "\u0000" + subtitle).hashCode() and 0x0FFFFFFF)
        messageNotificationIds.add(id)
        notifications.notify(id, notification)
    }

    /** Called when the user is looking at the app again. */
    fun clearMessageNotifications() {
        val ids = synchronized(messageNotificationIds) {
            val copy = messageNotificationIds.toList()
            messageNotificationIds.clear()
            copy
        }
        ids.forEach { notifications.cancel(it) }
    }

    private fun requireActivity(): MainActivity =
        activity ?: throw IOException("open IRCapybara to continue")

    private fun describe(uri: Uri): Pair<String, String> {
        var name = "attachment.bin"
        try {
            app.contentResolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)?.use { cursor ->
                if (cursor.moveToFirst() && !cursor.isNull(0)) name = cursor.getString(0)
            }
        } catch (_: Exception) {
            // Providers are free to refuse metadata queries; the fallback name is fine.
        }
        return name to (app.contentResolver.getType(uri) ?: "")
    }

    private fun copyBounded(uri: Uri, target: File, limit: Long) {
        val input = app.contentResolver.openInputStream(uri) ?: throw IOException("cannot read the selected file")
        input.use { source ->
            target.outputStream().use { sink ->
                val buffer = ByteArray(64 * 1024)
                var total = 0L
                while (true) {
                    val read = source.read(buffer)
                    if (read < 0) break
                    total += read
                    if (total > limit) throw IOException("the selected file is too large")
                    sink.write(buffer, 0, read)
                }
            }
        }
    }

    companion object {
        const val UPLOAD_DIR = "uploads"

        // The server's attachment limit is configurable up to 64 MiB; the core
        // enforces the real limit, this only stops a runaway copy.
        private const val MAX_UPLOAD_BYTES = 64L * 1024 * 1024
        private const val MAX_CERTIFICATE_BYTES = 1L * 1024 * 1024
    }
}
