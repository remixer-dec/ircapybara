(() => {
  'use strict';

  const { createApp, ref, reactive, computed, watch, nextTick, onMounted, onBeforeUnmount } = Vue;

  const blankSnapshot = () => ({
    state: 'disconnected',
    stateDetail: '',
    nick: '',
    server: '',
    extended: false,
    registered: false,
    identified: false,
    identityStatus: 'none',
    identityDetail: '',
    captchaWord: '',
    captchaPrompt: '',
    profiles: {},
    conversations: [{ id: 'status', name: 'Home', kind: 'status', messages: [] }],
  });

  // bridge.js hides whether Go is reached through Wails or the Android shell.
  const native = window.IRCapybaraNative;

  async function invoke(method, ...args) {
    return native.invoke(method, ...args);
  }

  // Below this width the sidebar and the conversation are separate panes.
  const phoneLayout = window.matchMedia('(max-width: 720px)');

  function errorText(err) {
    if (!err) return 'Something went wrong.';
    if (typeof err === 'string') return err;
    return err.message || String(err);
  }

  createApp({
    components: { ChatMarkdown: window.IRCapybaraMarkdown.component },
    setup() {
      const snapshot = ref(blankSnapshot());
      const activeId = ref('status');
      const viewMode = ref('conversation');
      const platform = ref(native.platform);
      const mobilePane = ref('list');
      const isPhone = ref(phoneLayout.matches);
      const actionMessage = ref(null);
      const awayFromLatest = ref(false);
      const arrivedBelow = ref(0);
      const theme = ref('light');
      const draft = ref('');
      const modal = ref('');
      const profileTarget = ref(null);
      const composerCursor = ref(0);
      const mentionIndex = ref(0);
      const mentionDismissed = ref(false);
      const formError = ref('');
      const busy = ref(false);
      const identityNick = ref('');
      const identityPassword = ref('');
      const captchaAnswer = ref('');
      const chatTarget = ref('');
      const helpTopic = ref('');
      const settingsTab = ref('appearance');
      const memberLoading = ref(false);
      const appFocused = ref(typeof document === 'undefined' ? true : (document.hasFocus() && !document.hidden));
      const messagesEl = ref(null);
      const messageInput = ref(null);
      const protocolEl = ref(null);
      const unread = reactive({});
      const downloadStates = reactive({});
      const toasts = ref([]);
      const protocolTrace = ref([]);
      const reconnectTargetId = ref('');
      const notificationPrefs = reactive({
        enabled: true,
        popup: true,
        sound: true,
        system: true,
        allChannels: true,
      });
      const appearancePrefs = reactive({ capybaraAvatars: false });
      const connection = reactive({
        host: '',
        port: 6697,
        nick: '',
        displayName: '',
        serverLabel: '',
        serverPassword: '',
        identityNick: '',
        identityPassword: '',
        autoJoinText: '#general',
        allowSelfSigned: true,
        pinServerCertificate: false,
        pinnedCertificatePath: '',
      });

      let eventCancels = [];
      let initialized = false;
      let toastSeq = 0;
      let previousIdentityStatus = 'none';
      let audioContext = null;
      let notificationAudio = null;
      let notificationAudioUnlocked = false;
      const lastMessageIds = new Map();
      const capybaraAssetFiles = ref([]);
      const capybaraAssets = reactive({});
      const capybaraCache = new Map();

      const conversations = computed(() => Array.isArray(snapshot.value.conversations) ? snapshot.value.conversations : []);
      const visibleConversations = computed(() => conversations.value.filter(c => c.kind !== 'status'));
      const channelConversations = computed(() => visibleConversations.value.filter(c => ['channel', 'private-channel', 'plugin-channel'].includes(c.kind)));
      const dmConversations = computed(() => visibleConversations.value.filter(c => ['dm', 'virtual-dm'].includes(c.kind)));
      const selectedConversation = computed(() => conversations.value.find(c => c.id === activeId.value) || conversations.value[0] || null);
      const isConnected = computed(() => snapshot.value.state !== 'disconnected');
      const canIdentify = computed(() => snapshot.value.registered && !snapshot.value.identified && snapshot.value.identityStatus !== 'pending' && snapshot.value.state !== 'captcha-pending');
      const canOpenChats = computed(() => snapshot.value.registered && snapshot.value.state !== 'captcha-pending');
      const selectedIsJoined = computed(() => {
        const conv = selectedConversation.value;
        return !conv || !isChannel(conv) || !!conv.joined;
      });
      const canCompose = computed(() => snapshot.value.state === 'ready' && selectedConversation.value && selectedConversation.value.kind !== 'status' && selectedIsJoined.value);
      const canAttach = computed(() => canCompose.value && snapshot.value.extended);
      const codePointCount = computed(() => Array.from(draft.value).length);
      const canSend = computed(() => canCompose.value && draft.value.length > 0 && codePointCount.value <= 32768);
      const canRequestHistory = computed(() => isConnected.value && selectedConversation.value && isChannel(selectedConversation.value) && !!selectedConversation.value.joined);
      const totalUnread = computed(() => visibleConversations.value.reduce((sum, conv) => sum + (Number(unread[conv.id]) || 0), 0));
      const canReconnect = computed(() => !isConnected.value && !!String(connection.host || '').trim() && !!String(connection.nick || '').trim());
      const themeTitle = computed(() => theme.value === 'dark' ? 'Switch to light theme' : 'Switch to dark theme');
      const isMobile = computed(() => platform.value !== 'desktop');
      // On a phone the conversation is only on screen while its pane is showing.
      const conversationVisible = computed(() => !isPhone.value || mobilePane.value === 'chat');
      const profileDetails = computed(() => buildProfileDetails(profileTarget.value?.name));

      const mentionSuggestions = computed(() => {
        if (mentionDismissed.value || !canCompose.value) return [];
        const context = mentionContext();
        if (!context) return [];
        const conv = selectedConversation.value;
        const candidates = new Map();
        for (const member of conv?.members || []) {
          const nick = String(member.nick || '').trim();
          if (nick) candidates.set(nick.toLowerCase(), { nick, operator: !!member.operator, online: member.online !== false });
        }
        for (const message of conv?.messages || []) {
          if (message.system || message.synthetic) continue;
          const nick = String(message.sender || '').trim();
          if (nick && !candidates.has(nick.toLowerCase())) candidates.set(nick.toLowerCase(), { nick, operator: false, online: true });
        }
        const ownNick = String(snapshot.value.nick || '').toLowerCase();
        const query = context.query.toLowerCase();
        if (isChannel(conv) && 'everyone'.includes(query)) {
          candidates.set('everyone', { nick: 'everyone', everyone: true, operator: false, online: true });
        }
        return [...candidates.values()]
          .filter(candidate => candidate.nick.toLowerCase() !== ownNick && candidate.nick.toLowerCase().includes(query))
          .sort((a, b) => {
            if (!!a.everyone !== !!b.everyone) return a.everyone ? -1 : 1;
            const aStarts = a.nick.toLowerCase().startsWith(query) ? 0 : 1;
            const bStarts = b.nick.toLowerCase().startsWith(query) ? 0 : 1;
            return aStarts - bStarts || a.nick.localeCompare(b.nick, undefined, { sensitivity: 'base' });
          })
          .slice(0, 8);
      });

      const serverLabel = computed(() => snapshot.value.server || connection.serverLabel || connection.host || 'IRCapybara');

      const statusLabel = computed(() => {
        const labels = {
          disconnected: 'Disconnected',
          connecting: 'Connecting',
          tls: 'Securing connection',
          'cap-negotiation': 'Negotiating',
          registered: 'Registering',
          'captcha-pending': 'Verification required',
          'identity-read-only': 'Connected · read-only',
          ready: 'Connected',
          'server-guard-read-only': 'Connected · guarded',
        };
        return labels[snapshot.value.state] || 'Connected';
      });

      const statusClass = computed(() => {
        if (snapshot.value.state === 'ready') return 'online';
        if (['connecting', 'tls', 'cap-negotiation', 'registered', 'captcha-pending'].includes(snapshot.value.state)) return 'connecting';
        if (['identity-read-only', 'server-guard-read-only'].includes(snapshot.value.state)) return 'readonly';
        return 'offline';
      });

      const identityLabel = computed(() => {
        if (!isConnected.value) return 'Not connected';
        if (snapshot.value.identityStatus === 'pending') return 'Identifying…';
        if (snapshot.value.identified) return 'Identified';
        if (snapshot.value.state === 'identity-read-only') return 'Read-only · Identify required';
        if (snapshot.value.state === 'server-guard-read-only') return 'Server guard · Read-only';
        if (snapshot.value.registered) return 'IRC session';
        return statusLabel.value;
      });

      const headerTitle = computed(() => viewMode.value === 'overview' ? 'All Chats' : (selectedConversation.value?.name || 'Home'));

      const headerSubtitle = computed(() => {
        if (viewMode.value === 'overview') return 'Channels and direct messages in one place';
        const conv = selectedConversation.value;
        if (!conv || conv.kind === 'status') return 'Connection, server notices, transfers, and help';
        if (conv.topic) return conv.topic;
        if (isChannel(conv) && !conv.joined) return 'Available channel · open it to join';
        return kindLabel(conv.kind);
      });

      const headerIcon = computed(() => viewMode.value === 'overview' ? 'chats' : conversationIcon(selectedConversation.value));

      const emptyTitle = computed(() => {
        if (!isConnected.value) return 'IRCapybara is ready';
        if (selectedConversation.value?.kind === 'status') return `Connected to ${serverLabel.value}`;
        if (isChannel(selectedConversation.value) && !selectedConversation.value.joined) return 'Open this channel to join';
        return 'No messages yet';
      });

      const emptyCopy = computed(() => {
        if (!isConnected.value) return 'Connect to a server to load its channel catalogue and conversations.';
        if (selectedConversation.value?.kind === 'status') return 'Server notices, transfer updates, identity feedback, and help output appear here.';
        if (isChannel(selectedConversation.value) && !selectedConversation.value.joined) return 'This channel came from the server catalogue. Selecting it joins only when you choose to open it.';
        return 'This conversation is quiet. New messages will appear here as plain text.';
      });

      const composerPlaceholder = computed(() => selectedConversation.value ? `Message ${selectedConversation.value.name}…` : 'Message…');
      const composerHint = computed(() => snapshot.value.extended
        ? 'Enter to send · Shift+Enter for a new line · Plain text · Attachments available · 32,768 characters max'
        : 'Enter to send · Shift+Enter for a new line · Plain text · 32,768 characters max');

      const banner = computed(() => {
        const state = snapshot.value.state;
        if (state === 'captcha-pending') return { visible: true, title: 'Quick verification required', text: snapshot.value.stateDetail || 'Complete the server challenge before channels are joined.', action: 'Verify', actionKind: 'captcha' };
        if (state === 'identity-read-only') return { visible: true, title: 'The server made this session read-only', text: snapshot.value.stateDetail || 'This restriction came from the server. Identify if this nickname belongs to a registered account.', action: 'Identify', actionKind: 'identify' };
        if (state === 'server-guard-read-only') return { visible: true, title: 'Server guard is active', text: snapshot.value.stateDetail || 'The server has temporarily limited this session to read-only access.', action: '', actionKind: '' };
        if (state === 'cap-negotiation') return { visible: true, title: 'Negotiating capabilities', text: snapshot.value.stateDetail || 'Preparing the IRC session…', action: 'Protocol log', actionKind: 'protocol' };
        if (['connecting', 'tls', 'registered'].includes(state)) return { visible: true, title: statusLabel.value, text: snapshot.value.stateDetail || 'Preparing your session…', action: '', actionKind: '' };
        return { visible: false, title: '', text: '', action: '', actionKind: '' };
      });

      function isChannel(conv) {
        return !!conv && ['channel', 'private-channel', 'plugin-channel'].includes(conv.kind);
      }

      function channelNickCandidates(conv) {
        const candidates = new Map();
        if (!isChannel(conv)) return candidates;
        for (const member of conv.members || []) {
          const nick = String(member.nick || '').trim();
          if (nick) candidates.set(nick.toLowerCase(), nick);
        }
        return candidates;
      }

      function isKnownChannelNick(conv, nick) {
        return !!String(nick || '').trim() && channelNickCandidates(conv).has(String(nick).trim().toLowerCase());
      }

      function showToast(text, kind = '', duration = 3200) {
        const id = ++toastSeq;
        toasts.value.push({ id, text: String(text || ''), kind, type: 'status' });
        window.setTimeout(() => {
          toasts.value = toasts.value.filter(t => t.id !== id);
        }, duration);
      }

      function showMessageToast(conv, msg, duration = 6000) {
        const id = ++toastSeq;
        toasts.value.push({
          id, type: 'message', kind: 'message',
          sender: String(msg.sender || 'Message'),
          conversation: String(conv.name || conv.id || ''),
          preview: messagePreview(msg),
        });
        window.setTimeout(() => {
          toasts.value = toasts.value.filter(t => t.id !== id);
        }, duration);
      }

      function compactCount(value) {
        const n = Number(value) || 0;
        return n > 99 ? '99+' : String(n);
      }

      function avatarLetters(name) {
        const value = String(name || '?').replace(/^#/, '').trim();
        if (!value) return '?';
        const parts = value.split(/[\s._-]+/).filter(Boolean);
        const letters = parts.length > 1 ? parts[0][0] + parts[1][0] : value.slice(0, 2);
        return letters.toUpperCase();
      }

      function kindLabel(kind) {
        const labels = {
          status: 'Server and status',
          channel: 'Public channel',
          'private-channel': 'Private channel',
          'plugin-channel': 'Plugin-backed channel',
          dm: 'Direct message',
          'virtual-dm': 'Virtual direct message',
        };
        return labels[kind] || 'Conversation';
      }

      function conversationIcon(conv) {
        if (!conv) return 'chat';
        if (conv.kind === 'status') return 'home';
        if (isChannel(conv)) return 'hash';
        if (conv.kind === 'virtual-dm') return 'plug';
        return 'user';
      }

      function hashName(value) {
        const text = String(value || '').toLowerCase();
        let hash = 2166136261;
        for (let i = 0; i < text.length; i++) {
          hash ^= text.charCodeAt(i);
          hash = Math.imul(hash, 16777619);
        }
        return hash >>> 0;
      }

      function seededValue(seed, salt) {
        let value = Math.imul((seed ^ salt) >>> 0, 0x45d9f3b);
        value = Math.imul(value ^ (value >>> 16), 0x45d9f3b);
        value ^= value >>> 16;
        return (value >>> 0) / 4294967296;
      }

      function hslToHex(hue, saturation, lightness) {
        const s = saturation / 100;
        const l = lightness / 100;
        const chroma = (1 - Math.abs(2 * l - 1)) * s;
        const section = (((hue % 360) + 360) % 360) / 60;
        const x = chroma * (1 - Math.abs(section % 2 - 1));
        const match = section < 1 ? [chroma, x, 0] : section < 2 ? [x, chroma, 0] : section < 3 ? [0, chroma, x] : section < 4 ? [0, x, chroma] : section < 5 ? [x, 0, chroma] : [chroma, 0, x];
        const offset = l - chroma / 2;
        return `#${match.map(channel => Math.round((channel + offset) * 255).toString(16).padStart(2, '0')).join('')}`;
      }

      function capybaraPalette(seed) {
        // Capybara fur stays in the natural brown/tan/gray range. Keep the
        // unusual cases to warm blonde and pale albino rather than turning
        // the animal blue or green.
        const furFamilies = [
          { hue: 18, saturation: 52, lightness: 34 },
          { hue: 24, saturation: 50, lightness: 42 },
          { hue: 30, saturation: 46, lightness: 48 },
          { hue: 36, saturation: 42, lightness: 56 },
          { hue: 42, saturation: 38, lightness: 63 },
          { hue: 50, saturation: 34, lightness: 51 },
          { hue: 58, saturation: 30, lightness: 44 },
        ];
        const family = furFamilies[Math.floor(seededValue(seed, 11) * furFamilies.length)];
        let furHue = family.hue + (seededValue(seed, 13) - 0.5) * 8;
        let furSaturation = family.saturation + (seededValue(seed, 43) - 0.5) * 12;
        let furLightness = family.lightness + (seededValue(seed, 47) - 0.5) * 16;
        const coatVariant = seededValue(seed, 151);
        if (coatVariant < 0.1) {
          // Albino/cream coat: warm enough to remain capybara-like, but very
          // low saturation and light enough to read as a pale animal.
          furHue = 34 + (seededValue(seed, 157) - 0.5) * 10;
          furSaturation = 6 + seededValue(seed, 163) * 14;
          furLightness = 78 + seededValue(seed, 167) * 12;
        } else if (coatVariant < 0.26) {
          // Blonde coat: golden beige, not bright yellow.
          furHue = 34 + (seededValue(seed, 173) - 0.5) * 12;
          furSaturation = 28 + seededValue(seed, 179) * 20;
          furLightness = 61 + seededValue(seed, 181) * 14;
        }
        furSaturation = Math.max(12, Math.min(65, furSaturation));
        furLightness = Math.max(27, Math.min(91, furLightness));
        const accentHue = (furHue + 76 + seededValue(seed, 23) * 205) % 360;
        const backgroundHue = seededValue(seed, 31) * 360;
        const outlineHue = (furHue + seededValue(seed, 71) * 34 - 17 + 360) % 360;
        return {
          bg: hslToHex(backgroundHue, 28 + seededValue(seed, 37) * 28, 81 + seededValue(seed, 41) * 11),
          fur: hslToHex(furHue, furSaturation, furLightness),
          shade: hslToHex(furHue, Math.min(80, furSaturation + 18), Math.max(20, furLightness - 18)),
          muzzle: hslToHex(furHue, Math.max(26, furSaturation - 2), Math.max(28, furLightness - 5)),
          ink: hslToHex(outlineHue, 36 + seededValue(seed, 73) * 30, 9 + seededValue(seed, 79) * 10),
          blush: hslToHex((furHue + 350 + seededValue(seed, 89) * 20) % 360, 24 + seededValue(seed, 83) * 22, 54 + seededValue(seed, 89) * 15),
          accent: hslToHex(accentHue, 48 + seededValue(seed, 97) * 34, 43 + seededValue(seed, 101) * 23),
          accent2: hslToHex((accentHue + 24 + seededValue(seed, 103) * 44) % 360, 34 + seededValue(seed, 107) * 34, 61 + seededValue(seed, 109) * 22),
          line: hslToHex(outlineHue, 44 + seededValue(seed, 113) * 30, 13 + seededValue(seed, 127) * 10),
          star: hslToHex((accentHue + 12) % 360, 62 + seededValue(seed, 137) * 26, 50 + seededValue(seed, 139) * 17),
        };
      }

      function recolorCapybara(source, seed) {
        const palette = capybaraPalette(seed);
        let svg = source;
        for (const className of ['bg', 'fur', 'shade', 'muzzle', 'ink', 'blush', 'accent', 'accent2']) {
          const classRule = new RegExp(`(\\.${className}\\s*\\{[^}]*?\\bfill:\\s*)#[0-9a-f]{3,8}`, 'i');
          svg = svg.replace(classRule, `$1${palette[className]}`);
        }
        svg = svg.replace(/(\.line\s*\{[^}]*?\bstroke:\s*)#[0-9a-f]{3,8}/i, `$1${palette.line}`);
        svg = svg.replace(/(\.star\s*\{[^}]*?\bstroke:\s*)#[0-9a-f]{3,8}/i, `$1${palette.star}`);
        // Artwork with explicit path strokes uses those as outlines too. Give
        // them the same nickname-seeded outline rather than one fixed brown.
        svg = svg.replace(/(stroke=["'])#[0-9a-f]{3,8}(["'])/gi, `$1${palette.line}$2`);
        return `data:image/svg+xml;charset=UTF-8,${encodeURIComponent(svg)}`;
      }

      function capybaraAvatar(name) {
        const seed = hashName(name);
        const files = capybaraAssetFiles.value;
        const styleIndex = files.length ? Math.floor(seededValue(seed, 0x6d2b79f5) * files.length) : 0;
        const style = files[styleIndex] || 'capybara-1.svg';
        const source = capybaraAssets[style];
        if (!source) return `./assets/capybara/${style}`;
        const cacheKey = `${style}:${seed}`;
        if (!capybaraCache.has(cacheKey)) capybaraCache.set(cacheKey, recolorCapybara(source, seed));
        return capybaraCache.get(cacheKey);
      }

      async function loadCapybaraAssets() {
        let files = [];
        try {
          const listed = await invoke('GetCapybaraAvatars');
          if (Array.isArray(listed)) files = listed.filter(file => typeof file === 'string' && file.toLowerCase().endsWith('.svg'));
        } catch (_) {}
        if (!files.length) files = ['capybara-1.svg'];
        capybaraAssetFiles.value = files;
        await Promise.all(files.map(async file => {
          try {
            const response = await fetch(`./assets/capybara/${file}`);
            if (response.ok) capybaraAssets[file] = await response.text();
          } catch (_) {}
        }));
      }

      function isOperator(message, conv = selectedConversation.value) {
        const sender = String(message?.sender || '').toLowerCase();
        return !!sender && !!conv && isChannel(conv) && (conv.members || []).some(member => member.operator && String(member.nick || '').toLowerCase() === sender);
      }

      function profileFromServer(name) {
        const profiles = snapshot.value.profiles && typeof snapshot.value.profiles === 'object' ? snapshot.value.profiles : {};
        const key = String(name || '').toLowerCase();
        return profiles[key] || Object.values(profiles).find(profile => String(profile?.nick || '').toLowerCase() === key) || null;
      }

      function usableProfileDate(value) {
        if (!value) return '';
        const date = new Date(value);
        // Go time.Time zero values are encoded as year 1. They are absence,
        // not a real sign-on or activity date.
        if (Number.isNaN(date.getTime()) || date.getTime() <= 0) return '';
        return value;
      }

      function buildProfileDetails(name) {
        const value = String(name || '').trim();
        if (!value) return null;
        const key = value.toLowerCase();
        const channels = new Map();
        let latestMessage = '';
        let messageCount = 0;
        let localOnline = false;
        let localOperator = false;
        for (const conv of conversations.value) {
          const member = (conv.members || []).find(item => String(item.nick || '').toLowerCase() === key);
          if (member) {
            localOnline = localOnline || member.online !== false;
            localOperator = localOperator || !!member.operator;
            if (isChannel(conv)) channels.set(conv.id.toLowerCase(), conv.name || conv.id);
          }
          for (const message of conv.messages || []) {
            if (String(message.sender || '').toLowerCase() !== key) continue;
            messageCount += 1;
            const messageDate = new Date(message.time);
            if (messageDate.getTime() > 0 && (!latestMessage || messageDate > new Date(latestMessage))) latestMessage = message.time;
            if (isChannel(conv)) channels.set(conv.id.toLowerCase(), conv.name || conv.id);
          }
        }
        const serverProfile = profileFromServer(value) || {};
        const serverLastActive = usableProfileDate(serverProfile.lastActive);
        const serverSignOn = usableProfileDate(serverProfile.signOn);
        const remoteChannels = Array.isArray(serverProfile.channels) ? serverProfile.channels : [];
        for (const channel of remoteChannels) if (channel) channels.set(String(channel).toLowerCase(), channel);
        return {
          name: serverProfile.nick || value,
          username: serverProfile.username || '',
          host: serverProfile.host || '',
          realname: serverProfile.realname || '',
          channels: [...channels.values()],
          operator: !!serverProfile.operator || localOperator,
          online: isConnected.value && (!!serverProfile.online || localOnline),
          loading: !!serverProfile.loading,
          error: serverProfile.error || profileTarget.value?.requestError || '',
          idleSeconds: Number(serverProfile.idleSeconds) || 0,
          lastActive: serverLastActive,
          lastSeen: isConnected.value && serverLastActive ? serverLastActive : latestMessage,
          signOn: serverSignOn,
          messageCount,
        };
      }

      function profilePresence(profile) {
        if (!profile) return '';
        if (profile.loading) return 'Checking live status…';
        if (profile.online) {
          if (profile.idleSeconds > 60) return `Online · idle for ${formatDuration(profile.idleSeconds)}`;
          return 'Online now';
        }
        return 'Offline';
      }

      function profileLastSeen(profile) {
        if (!profile) return 'Not available';
        if (profile.online) {
          if (profile.lastActive) return `Active ${formatRelativeTime(profile.lastActive)}`;
          return 'Online now';
        }
        if (profile.lastSeen) return `${formatDateTime(profile.lastSeen)} · local history`;
        return 'Not available';
      }

      async function openProfile(name) {
        const value = String(name || '').trim();
        if (!value) return;
        profileTarget.value = { name: value, operator: isOperator({ sender: value }), requestError: '' };
        openModal('profile');
        if (isConnected.value) {
          try {
            await invoke('RequestProfile', value);
          } catch (err) {
            if (profileTarget.value?.name.toLowerCase() === value.toLowerCase()) profileTarget.value.requestError = errorText(err);
          }
        }
      }

      function mentionContext() {
        const cursor = Math.max(0, Math.min(Number(composerCursor.value) || 0, draft.value.length));
        const before = draft.value.slice(0, cursor);
        const match = before.match(/(?:^|\s)@([A-Za-z0-9_\-\[\]\\`^{}]*)$/);
        if (!match) return null;
        return { start: cursor - match[1].length - 1, query: match[1] };
      }

      function syncComposerCursor(event) {
        if (event?.target && typeof event.target.selectionStart === 'number') composerCursor.value = event.target.selectionStart;
      }

      function handleComposerInput(event) {
        mentionDismissed.value = false;
        syncComposerCursor(event);
        normalizeDraft();
      }

      function moveMention(delta) {
        if (!mentionSuggestions.value.length) return;
        const count = mentionSuggestions.value.length;
        mentionIndex.value = (mentionIndex.value + delta + count) % count;
      }

      function dismissMentions() {
        if (mentionSuggestions.value.length) mentionDismissed.value = true;
      }

      function selectMention(nick = mentionSuggestions.value[mentionIndex.value]?.nick) {
        if (!nick) return;
        const context = mentionContext();
        if (!context) return;
        const cursor = Math.max(0, Math.min(Number(composerCursor.value) || 0, draft.value.length));
        const before = draft.value.slice(0, context.start);
        const after = draft.value.slice(cursor);
        let replacement = `@${nick} `;
        if (String(nick).toLowerCase() === 'everyone' && isChannel(selectedConversation.value)) {
          const ownNick = String(snapshot.value.nick || '').trim().toLowerCase();
          const everyone = [...channelNickCandidates(selectedConversation.value).entries()]
            .filter(([key]) => key !== ownNick)
            .map(([, value]) => `@${value}`);
          if (everyone.length) replacement = `${everyone.join(' ')} `;
        }
        draft.value = `${before}${replacement}${after}`;
        mentionDismissed.value = false;
        mentionIndex.value = 0;
        const nextCursor = before.length + replacement.length;
        composerCursor.value = nextCursor;
        nextTick(() => {
          const input = messageInput.value;
          if (!input) return;
          input.focus();
          input.setSelectionRange(nextCursor, nextCursor);
        });
      }

      function handleComposerKeydown(event) {
        if (event.isComposing) return;
        if (event.key === 'ArrowDown' && mentionSuggestions.value.length) {
          event.preventDefault();
          moveMention(1);
          return;
        }
        if (event.key === 'ArrowUp' && mentionSuggestions.value.length) {
          event.preventDefault();
          moveMention(-1);
          return;
        }
        if (event.key === 'Tab' && mentionSuggestions.value.length) {
          event.preventDefault();
          selectMention();
          return;
        }
        if (event.key === 'Escape' && mentionSuggestions.value.length) {
          event.preventDefault();
          dismissMentions();
          return;
        }
        if (event.key === 'Enter' && !event.shiftKey && !event.altKey && !event.ctrlKey && !event.metaKey) {
          event.preventDefault();
          if (mentionSuggestions.value.length) selectMention();
          else sendMessage();
        }
      }

      async function openProfileDM() {
        const target = profileTarget.value?.name;
        if (!target) return;
        const ok = await runForm(() => invoke('OpenDirectMessage', target));
        if (ok) {
          closeModal();
          openConversation(target);
        }
      }

      function formatTime(value) {
        if (!value) return '';
        const date = new Date(value);
        if (Number.isNaN(date.getTime())) return '';
        return new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit' }).format(date);
      }

      function formatDateTime(value) {
        if (!value) return '';
        const date = new Date(value);
        if (Number.isNaN(date.getTime())) return '';
        return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(date);
      }

      function formatDuration(seconds) {
        const total = Math.max(0, Number(seconds) || 0);
        if (total < 60) return `${Math.round(total)} sec`;
        if (total < 3600) return `${Math.floor(total / 60)} min`;
        if (total < 86400) return `${Math.floor(total / 3600)} hr`;
        return `${Math.floor(total / 86400)} d`;
      }

      function formatRelativeTime(value) {
        const date = new Date(value);
        if (Number.isNaN(date.getTime())) return 'recently';
        const seconds = Math.max(0, (Date.now() - date.getTime()) / 1000);
        if (seconds < 60) return 'just now';
        return `${formatDuration(seconds)} ago`;
      }

      function formatTraceTime(value) {
        if (!value) return '';
        const date = new Date(value);
        if (Number.isNaN(date.getTime())) return '';
        return new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit', fractionalSecondDigits: 3 }).format(date);
      }

      function dateKey(value) {
        const d = new Date(value);
        if (Number.isNaN(d.getTime())) return '';
        return `${d.getFullYear()}-${d.getMonth()}-${d.getDate()}`;
      }

      function showDaySeparator(index, messages) {
        if (!messages || !messages[index]) return false;
        if (index === 0) return true;
        return dateKey(messages[index - 1].time) !== dateKey(messages[index].time);
      }

      function formatDay(value) {
        const d = new Date(value);
        if (Number.isNaN(d.getTime())) return '';
        const now = new Date();
        const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
        const thatDay = new Date(d.getFullYear(), d.getMonth(), d.getDate());
        const diff = Math.round((today - thatDay) / 86400000);
        if (diff === 0) return 'Today';
        if (diff === 1) return 'Yesterday';
        return new Intl.DateTimeFormat(undefined, { weekday: 'long', month: 'short', day: 'numeric', year: d.getFullYear() === now.getFullYear() ? undefined : 'numeric' }).format(d);
      }

      function appendMentionSegments(value, start, end, conv, out) {
        let cursor = start;
        const plain = value.slice(start, end);
        const re = /(^|[\s([{\"'!?.,;:])(@[A-Za-z][A-Za-z0-9_\-\[\]\\`^{}]{0,30})(?![A-Za-z0-9_\-\[\]\\`^{}])/g;
        let match;
        while ((match = re.exec(plain)) !== null) {
          const mentionStart = start + match.index + match[1].length;
          const mentionEnd = mentionStart + match[2].length;
          if (mentionStart > cursor) out.push({ text: value.slice(cursor, mentionStart), url: '' });
          const nick = match[2].slice(1);
          if (isKnownChannelNick(conv, nick)) out.push({ text: match[2], url: '', mention: nick });
          else out.push({ text: match[2], url: '' });
          cursor = mentionEnd;
        }
        if (cursor < end) out.push({ text: value.slice(cursor, end), url: '' });
      }

      function linkifyText(text, conv) {
        const value = String(text || '');
        const out = [];
        const re = /https?:\/\/[^\s<>"']+/gi;
        let cursor = 0;
        let match;
        while ((match = re.exec(value)) !== null) {
          if (match.index > cursor) appendMentionSegments(value, cursor, match.index, conv, out);
          let url = match[0];
          let suffix = '';
          while (/[.,!?;:]$/.test(url)) {
            suffix = url.slice(-1) + suffix;
            url = url.slice(0, -1);
          }
          if (url.endsWith(')') && (url.match(/\(/g) || []).length < (url.match(/\)/g) || []).length) {
            suffix = ')' + suffix;
            url = url.slice(0, -1);
          }
          out.push({ text: url, url });
          if (suffix) appendMentionSegments(suffix, 0, suffix.length, conv, out);
          cursor = match.index + match[0].length;
        }
        if (cursor < value.length) appendMentionSegments(value, cursor, value.length, conv, out);
        return out.length ? out : [{ text: value, url: '' }];
      }

      function openExternalLink(url) {
        if (!/^https?:\/\//i.test(String(url || ''))) return;
        native.openExternal(url);
      }

      function formatBytes(value) {
        const bytes = Number(value) || 0;
        if (bytes < 1024) return `${bytes} B`;
        const units = ['KiB', 'MiB', 'GiB'];
        let n = bytes / 1024;
        let unit = units[0];
        for (let i = 1; i < units.length && n >= 1024; i++) {
          n /= 1024;
          unit = units[i];
        }
        return `${n >= 10 ? n.toFixed(0) : n.toFixed(1)} ${unit}`;
      }

      function fileBadge(mime) {
        const value = String(mime || '').toLowerCase();
        if (value.startsWith('image/')) return 'IMG';
        if (value.startsWith('video/')) return 'VID';
        if (value.startsWith('audio/')) return 'AUD';
        if (value.includes('pdf')) return 'PDF';
        if (value.startsWith('text/')) return 'TXT';
        return 'FILE';
      }

      function lastPreview(conv) {
        if (!conv || !Array.isArray(conv.messages) || !conv.messages.length) return conv?.joined ? 'No messages yet' : 'Available channel';
        const msg = conv.messages[conv.messages.length - 1];
        if (msg.attachment) return `${msg.sender}: ${msg.attachment.filename}`;
        const text = String(msg.text || '').replace(/\s+/g, ' ').trim();
        return text ? `${msg.sender}: ${text}` : `${msg.sender} sent an update`;
      }

      function messagePreview(msg) {
        if (msg.attachment) return msg.attachment.filename || 'Attachment';
        const text = String(msg.text || '').replace(/\s+/g, ' ').trim();
        return text.length > 90 ? `${text.slice(0, 87)}…` : text;
      }

      function isMention(msg, snap) {
        const nick = String(snap.nick || '').trim().toLowerCase();
        if (!nick) return false;
        const re = /(?:^|[\s([{\"'!?.,;:])@([A-Za-z][A-Za-z0-9_\-\[\]\\`^{}]{0,30})(?![A-Za-z0-9_\-\[\]\\`^{}])/gi;
        let match;
        while ((match = re.exec(String(msg.text || ''))) !== null) {
          if (match[1].toLowerCase() === nick) return true;
        }
        return false;
      }

      function shouldNotify(conv, msg, snap) {
        if (!notificationPrefs.enabled || !msg || msg.historical || msg.system || String(msg.sender || '').toLowerCase() === String(snap.nick || '').toLowerCase()) return false;
        const dm = ['dm', 'virtual-dm'].includes(conv.kind);
        return dm || (isChannel(conv) && isMention(msg, snap)) || (isChannel(conv) && notificationPrefs.allChannels);
      }

      function ensureAudioContext() {
        if (!audioContext) {
          const Ctx = window.AudioContext || window.webkitAudioContext;
          if (Ctx) audioContext = new Ctx();
        }
        if (audioContext?.state === 'suspended') audioContext.resume().catch(() => {});
        return audioContext;
      }

      function ensureNotificationAudio() {
        if (!notificationAudio) {
          notificationAudio = new Audio('./assets/notify.wav');
          notificationAudio.preload = 'auto';
          notificationAudio.volume = 0.72;
        }
        return notificationAudio;
      }

      function unlockNotificationAudio() {
        if (notificationAudioUnlocked) return;
        const audio = ensureNotificationAudio();
        const oldVolume = audio.volume;
        audio.volume = 0;
        const promise = audio.play();
        if (promise && typeof promise.then === 'function') {
          promise.then(() => {
            audio.pause();
            audio.currentTime = 0;
            audio.volume = oldVolume;
            notificationAudioUnlocked = true;
          }).catch(() => { audio.volume = oldVolume; });
        } else {
          audio.pause();
          audio.currentTime = 0;
          audio.volume = oldVolume;
          notificationAudioUnlocked = true;
        }
        ensureAudioContext();
      }

      function playNotificationSound() {
        if (!notificationPrefs.sound) return;
        const audio = ensureNotificationAudio();
        try {
          audio.pause();
          audio.currentTime = 0;
          audio.volume = 0.72;
          const promise = audio.play();
          if (promise && typeof promise.catch === 'function') {
            promise.catch(() => playNotificationToneFallback());
            return;
          }
          return;
        } catch (_) {}
        playNotificationToneFallback();
      }

      function playNotificationToneFallback() {
        const ctx = ensureAudioContext();
        if (!ctx || ctx.state !== 'running') return;
        try {
          const osc = ctx.createOscillator();
          const gain = ctx.createGain();
          const now = ctx.currentTime;
          osc.frequency.setValueAtTime(660, now);
          osc.frequency.exponentialRampToValueAtTime(520, now + 0.12);
          gain.gain.setValueAtTime(0.0001, now);
          gain.gain.exponentialRampToValueAtTime(0.065, now + 0.012);
          gain.gain.exponentialRampToValueAtTime(0.0001, now + 0.17);
          osc.connect(gain);
          gain.connect(ctx.destination);
          osc.start(now);
          osc.stop(now + 0.18);
        } catch (_) {}
      }

      function processIncoming(conv, msg, snap) {
        if (!msg || msg.historical || msg.system || conv.kind === 'status' || String(msg.sender || '').toLowerCase() === String(snap.nick || '').toLowerCase()) return;
        const focused = document.hasFocus() && !document.hidden;
        const activelyReading = viewMode.value === 'conversation' && activeId.value === conv.id && focused && conversationVisible.value;
        const notify = shouldNotify(conv, msg, snap);
        if (!activelyReading) unread[conv.id] = (unread[conv.id] || 0) + 1;
        if (notificationPrefs.enabled && notificationPrefs.sound && focused) playNotificationSound();
        if (!activelyReading && notify) {
          if (focused && notificationPrefs.popup) showMessageToast(conv, msg);
          if (!focused && notificationPrefs.system) {
            const preview = messagePreview(msg);
            invoke('SendSystemNotification', String(msg.sender || 'IRCapybara'), String(conv.name || ''), preview || 'New message', !!notificationPrefs.sound).catch(() => {});
          }
        }
      }

      function rememberLatest(snap, countUnread) {
        for (const conv of snap.conversations || []) {
          const messages = Array.isArray(conv.messages) ? conv.messages : [];
          const latest = messages[messages.length - 1];
          const previousId = lastMessageIds.get(conv.id);
          if (latest && previousId && latest.id !== previousId && conv.id === activeId.value && awayFromLatest.value) arrivedBelow.value += 1;
          if (countUnread && latest && latest.id !== previousId) {
            let newMessages = [latest];
            if (previousId) {
              const idx = messages.findIndex(m => m.id === previousId);
              if (idx >= 0) newMessages = messages.slice(idx + 1);
            }
            for (const msg of newMessages) processIncoming(conv, msg, snap);
          }
          if (latest?.id) lastMessageIds.set(conv.id, latest.id);
        }
      }

      function normalizeSnapshot(raw) {
        const next = raw && typeof raw === 'object' ? raw : blankSnapshot();
        if (!Array.isArray(next.conversations)) next.conversations = [];
        if (!next.profiles || typeof next.profiles !== 'object') next.profiles = {};
        if (!next.conversations.some(c => c.id === 'status')) next.conversations.unshift({ id: 'status', name: 'Home', kind: 'status', messages: [] });
        for (const conv of next.conversations) if (!Array.isArray(conv.messages)) conv.messages = [];
        if (!next.identityStatus) next.identityStatus = next.identified ? 'identified' : 'none';
        return next;
      }

      function applySnapshot(raw, countUnread = true) {
        const next = normalizeSnapshot(raw);
        if (initialized && snapshot.value.state === 'disconnected' && next.state !== 'disconnected') {
          for (const key of Object.keys(unread)) delete unread[key];
          lastMessageIds.clear();
        }
        rememberLatest(next, initialized && countUnread);

        const nextIdentityStatus = next.identityStatus || 'none';
        if (initialized && nextIdentityStatus !== previousIdentityStatus) {
          if (nextIdentityStatus === 'identified') showToast('Identity confirmed by the server.', 'success');
          if (nextIdentityStatus === 'failed') showToast(next.identityDetail || 'Identity verification failed.', 'error', 4800);
        }
        previousIdentityStatus = nextIdentityStatus;

        snapshot.value = next;
        initialized = true;

        const restored = reconnectTargetId.value && next.conversations.some(c => c.id === reconnectTargetId.value);
        if (restored) {
          activeId.value = reconnectTargetId.value;
          reconnectTargetId.value = '';
          viewMode.value = 'conversation';
          unread[activeId.value] = 0;
        } else if (!reconnectTargetId.value && !next.conversations.some(c => c.id === activeId.value)) {
          activeId.value = 'status';
        }
        if (next.captchaWord && modal.value !== 'captcha') {
          modal.value = 'captcha';
          formError.value = '';
          captchaAnswer.value = '';
        } else if (!next.captchaWord && modal.value === 'captcha') {
          closeModal();
        }
        if (viewMode.value === 'conversation') scrollMessages(false);
      }

      async function openConversation(id) {
        if (id !== reconnectTargetId.value) reconnectTargetId.value = '';
        const conv = conversations.value.find(c => c.id === id);
        activeId.value = id;
        viewMode.value = 'conversation';
        mobilePane.value = 'chat';
        awayFromLatest.value = false;
        arrivedBelow.value = 0;
        unread[id] = 0;
        if (conv && isChannel(conv) && !conv.joined && isConnected.value) {
          try {
            await invoke('Join', conv.id);
          } catch (err) {
            showToast(errorText(err), 'error');
          }
        }
        nextTick(() => scrollMessages(true));
      }

      async function closeConversation(conv) {
        if (!conv || conv.kind === 'status') return;
        try {
          await invoke('CloseConversation', conv.id);
          delete unread[conv.id];
          lastMessageIds.delete(conv.id);
          if (activeId.value === conv.id) {
            activeId.value = 'status';
            viewMode.value = 'conversation';
            mobilePane.value = 'list';
          }
        } catch (err) {
          showToast(errorText(err), 'error');
        }
      }

      function openOverview() {
        viewMode.value = 'overview';
        mobilePane.value = 'chat';
      }

      function showList() {
        mobilePane.value = 'list';
      }

      // Platform back gesture: close the topmost layer and report whether there
      // was one. The captcha dialog is mandatory and cannot be dismissed.
      function goBack() {
        if (modal.value && modal.value !== 'captcha') {
          closeModal();
          return true;
        }
        if (isPhone.value && mobilePane.value === 'chat') {
          showList();
          return true;
        }
        return false;
      }

      function scrollMessages(force) {
        nextTick(() => {
          const el = messagesEl.value;
          if (!el) return;
          const distance = el.scrollHeight - el.scrollTop - el.clientHeight;
          if (force || distance < 180) el.scrollTop = el.scrollHeight;
        });
      }

      // Tracks whether the reader has scrolled away from the newest message, so
      // the jump control can appear and new arrivals do not yank the view.
      function onMessagesScroll() {
        const el = messagesEl.value;
        if (!el) return;
        const away = el.scrollHeight - el.scrollTop - el.clientHeight > 320;
        awayFromLatest.value = away;
        if (!away) arrivedBelow.value = 0;
      }

      function jumpToLatest() {
        const el = messagesEl.value;
        if (!el) return;
        el.scrollTo({ top: el.scrollHeight, behavior: 'smooth' });
        arrivedBelow.value = 0;
      }

      function firstLink(text) {
        const link = linkifyText(text).find(segment => segment.url);
        return link ? link.url : '';
      }

      function isOwnMessage(message) {
        return String(message?.sender || '').toLowerCase() === String(snapshot.value.nick || '').toLowerCase();
      }

      // Long-press (touch) or right-click (mouse) on a message.
      function openMessageActions(message) {
        if (!message || !message.text || modal.value) return;
        actionMessage.value = message;
        native.haptic('longpress');
        openModal('message');
      }

      function messageFromNode(node) {
        const id = node?.dataset?.messageId;
        return (selectedConversation.value?.messages || []).find(m => m.id === id) || null;
      }

      async function copyText(text, done) {
        try {
          await native.copyText(text);
          showToast(done);
        } catch (_) {
          showToast('Could not copy to the clipboard.', 'error');
        }
      }

      function copyCode(source) {
        return copyText(source, 'Code copied.');
      }

      function copyActionMessage(link) {
        const message = actionMessage.value;
        if (!message) return;
        closeModal();
        if (link) copyText(firstLink(message.text), 'Link copied.');
        else copyText(message.text, 'Message copied.');
      }

      function mentionActionSender() {
        const message = actionMessage.value;
        if (!message) return;
        closeModal();
        const prefix = `@${message.sender} `;
        if (!draft.value.startsWith(prefix)) draft.value = prefix + draft.value;
        normalizeDraft();
        nextTick(() => messageInput.value?.focus());
      }

      async function messageActionSender() {
        const message = actionMessage.value;
        if (!message) return;
        closeModal();
        try {
          await invoke('OpenDirectMessage', message.sender);
          openConversation(message.sender);
        } catch (err) {
          showToast(errorText(err), 'error');
        }
      }

      function scrollProtocol() {
        nextTick(() => {
          const el = protocolEl.value;
          if (el) el.scrollTop = el.scrollHeight;
        });
      }

      function openModal(name) {
        formError.value = '';
        busy.value = false;
        modal.value = name;
      }

      function closeModal() {
        modal.value = '';
        profileTarget.value = null;
        formError.value = '';
        busy.value = false;
      }

      function openSettings(tab = 'appearance') {
        settingsTab.value = tab;
        openModal('settings');
      }

      async function openMembers() {
        const conv = selectedConversation.value;
        if (!conv || !isChannel(conv) || !conv.joined) return;
        openModal('members');
        await refreshMembers();
      }

      async function refreshMembers() {
        const conv = selectedConversation.value;
        if (!conv || !isChannel(conv) || !conv.joined || memberLoading.value) return;
        memberLoading.value = true;
        try {
          await invoke('RequestMembers', conv.id);
        } catch (err) {
          showToast(errorText(err), 'error');
        } finally {
          window.setTimeout(() => { memberLoading.value = false; }, 250);
        }
      }

      async function runForm(action) {
        formError.value = '';
        busy.value = true;
        try {
          await action();
          return true;
        } catch (err) {
          formError.value = errorText(err);
          return false;
        } finally {
          busy.value = false;
        }
      }

      async function connect() {
        const cfg = {
          host: connection.host,
          port: Number(connection.port) || 6697,
          nick: connection.nick,
          displayName: connection.displayName,
          serverLabel: connection.serverLabel,
          serverPassword: connection.serverPassword,
          identityNick: connection.identityNick,
          identityPassword: connection.identityPassword,
          autoJoin: connection.autoJoinText.split(',').map(v => v.trim()).filter(Boolean),
          allowSelfSigned: !!connection.allowSelfSigned,
          pinServerCertificate: !!connection.pinServerCertificate,
          pinnedCertificatePath: connection.pinnedCertificatePath,
        };
        const ok = await runForm(() => invoke('Connect', cfg));
        if (ok) {
          connection.serverPassword = '';
          connection.identityPassword = '';
          closeModal();
          openConversation('status');
        }
      }

      async function disconnect() {
        try {
          await invoke('Disconnect');
          showToast('Disconnected.');
        } catch (err) {
          showToast(errorText(err), 'error');
        }
      }

      async function reconnect() {
        if (!canReconnect.value) {
          openModal('connect');
          return;
        }
        reconnectTargetId.value = viewMode.value === 'conversation' && isChannel(selectedConversation.value) ? activeId.value : '';
        formError.value = '';
        busy.value = true;
        const cfg = {
          host: connection.host,
          port: Number(connection.port) || 6697,
          nick: connection.nick,
          displayName: connection.displayName,
          serverLabel: connection.serverLabel,
          serverPassword: connection.serverPassword,
          identityNick: connection.identityNick,
          identityPassword: connection.identityPassword,
          autoJoin: connection.autoJoinText.split(',').map(v => v.trim()).filter(Boolean),
          allowSelfSigned: !!connection.allowSelfSigned,
          pinServerCertificate: !!connection.pinServerCertificate,
          pinnedCertificatePath: connection.pinnedCertificatePath,
        };
        try {
          await invoke('Connect', cfg);
          connection.serverPassword = '';
          connection.identityPassword = '';
          showToast(`Reconnecting to ${connection.serverLabel || connection.host}…`);
        } catch (err) {
          formError.value = errorText(err);
          openModal('connect');
        } finally {
          busy.value = false;
        }
      }

      async function identify() {
        const nick = identityNick.value.trim();
        const ok = await runForm(() => nick ? invoke('IdentifyAs', nick, identityPassword.value) : invoke('Identify', identityPassword.value));
        if (ok) {
          identityNick.value = '';
          identityPassword.value = '';
          closeModal();
          showToast('Identity request sent. Waiting for the server to confirm it.');
        }
      }

      async function solveCaptcha() {
        const word = snapshot.value.captchaWord;
        const ok = await runForm(() => invoke('SolveCaptcha', word, captchaAnswer.value));
        if (ok) {
          captchaAnswer.value = '';
          showToast('Challenge response sent.');
        }
      }

      async function openChat() {
        const target = chatTarget.value.trim();
        const ok = await runForm(async () => {
          if (target.startsWith('#')) await invoke('Join', target);
          else await invoke('OpenDirectMessage', target);
        });
        if (ok) {
          chatTarget.value = '';
          closeModal();
          openConversation(target);
        }
      }

      async function requestHistory() {
        const conv = selectedConversation.value;
        if (!conv) return;
        try {
          await invoke('RequestHistory', conv.id, 0);
          showToast(`Loading recent history for ${conv.name}…`);
        } catch (err) {
          showToast(errorText(err), 'error');
        }
      }

      async function refreshChannels() {
        try {
          await invoke('RefreshChannelList');
          showToast('Refreshing the server channel list…');
        } catch (err) {
          showToast(errorText(err), 'error');
        }
      }

      async function requestHelp() {
        const topic = helpTopic.value;
        const ok = await runForm(() => invoke('RequestHelp', topic));
        if (ok) {
          helpTopic.value = '';
          closeModal();
          openConversation('status');
        }
      }

      async function resetAI() {
        try {
          await invoke('ResetAI');
          showToast('AI conversation reset requested.');
        } catch (err) {
          showToast(errorText(err), 'error');
        }
      }

      function normalizeDraft() {
        nextTick(() => {
          const el = messageInput.value;
          if (!el) return;
          // WebKit changes textarea baseline/scroll metrics after the first edit.
          // Reset to the explicit single-line box before measuring so it never
          // jumps vertically when content changes.
          el.style.height = '38px';
          const nextHeight = Math.max(38, Math.min(el.scrollHeight, 128));
          el.style.height = `${nextHeight}px`;
          el.style.overflowY = el.scrollHeight > 128 ? 'auto' : 'hidden';
        });
      }

      async function sendMessage() {
        if (!canSend.value) return;
        const conv = selectedConversation.value;
        if (!conv) return;
        const text = draft.value;
        try {
          await invoke('SendMessage', conv.id, text);
          draft.value = '';
          composerCursor.value = 0;
          mentionDismissed.value = false;
          mentionIndex.value = 0;
          normalizeDraft();
          nextTick(() => messageInput.value?.focus());
        } catch (err) {
          showToast(errorText(err), 'error');
        }
      }

      async function pickAndUpload() {
        const conv = selectedConversation.value;
        if (!conv) return;
        try {
          await invoke('PickAndUpload', conv.id);
        } catch (err) {
          showToast(errorText(err), 'error');
        }
      }

      function transferState(attachment) {
        const fileId = attachment?.fileId;
        return (fileId && downloadStates[fileId]) || { status: 'available', percent: 0, received: 0, total: Number(attachment?.size) || 0 };
      }

      function transferIsBusy(attachment) {
        return ['queued', 'downloading', 'receiving-missing', 'verifying', 'verified'].includes(transferState(attachment).status);
      }

      function transferStatusLabel(attachment) {
        const state = transferState(attachment);
        const labels = {
          queued: 'Queued',
          downloading: 'Downloading',
          'receiving-missing': 'Receiving missing parts',
          verifying: 'Verifying',
          verified: 'Acknowledging',
          complete: 'Complete',
          interrupted: 'Interrupted',
          'retry-available': 'Retry available',
          expired: 'Expired',
          unauthorized: 'Unauthorized',
        };
        return labels[state.status] || '';
      }

      function transferButtonLabel(attachment) {
        const state = transferState(attachment);
        if (state.status === 'downloading' || state.status === 'receiving-missing') return `${Math.max(0, Math.min(100, Number(state.percent) || 0))}%`;
        if (state.status === 'queued') return 'Queued';
        if (state.status === 'verifying') return 'Verifying';
        if (state.status === 'verified') return 'Finishing';
        if (state.status === 'complete') return isMobile.value ? 'Open' : 'Show in Folder';
        if (state.status === 'interrupted' || state.status === 'retry-available') return 'Retry';
        if (state.status === 'expired' || state.status === 'unauthorized') return 'Unavailable';
        return 'Download';
      }

      function transferButtonIcon(attachment) {
        const state = transferState(attachment);
        if (state.status === 'complete') return isMobile.value ? 'arrow-up-right' : 'folder';
        if (state.status === 'interrupted' || state.status === 'retry-available') return 'refresh';
        return 'download';
      }

      async function downloadAttachment(attachment) {
        const state = transferState(attachment);
        if (transferIsBusy(attachment) || state.status === 'expired' || state.status === 'unauthorized') return;
        try {
          if (state.status === 'complete') {
            await invoke('RevealDownloadedFile', attachment.fileId);
            return;
          }
          if (state.status === 'interrupted' || state.status === 'retry-available') {
            await invoke('RetryDownload', attachment.fileId, false);
            return;
          }
          await invoke('DownloadAttachment', attachment.fileId);
        } catch (err) {
          const text = errorText(err);
          if (/no longer at its saved location/i.test(text)) {
            downloadStates[attachment.fileId] = { status: 'available', total: Number(attachment.size) || 0, percent: 0 };
          }
          showToast(text, 'error');
        }
      }

      async function restartAttachment(attachment) {
        try {
          await invoke('RetryDownload', attachment.fileId, true);
        } catch (err) {
          showToast(errorText(err), 'error');
        }
      }

      function handleTransferUpdate(update) {
        if (!update || !update.fileId) return;
        const previous = downloadStates[update.fileId];
        downloadStates[update.fileId] = { ...update };
        if (update.status === 'complete' && previous?.status !== 'complete') showToast(isMobile.value ? 'Download complete. The file is in your Downloads folder.' : 'Download complete. The file is ready in its saved folder.', 'success', 4800);
        if (update.status === 'retry-available' && previous?.status !== 'retry-available') showToast(update.error || 'Download needs attention. Retry is available.', 'error', 5200);
        if ((update.status === 'expired' || update.status === 'unauthorized') && previous?.status !== update.status) showToast(update.error || 'This attachment is no longer available.', 'error', 5200);
      }

      async function pickTLSCertificate() {
        try {
          const path = await invoke('PickTLSCertificate');
          if (path) connection.pinnedCertificatePath = path;
        } catch (err) {
          showToast(errorText(err), 'error');
        }
      }

      function applyTheme(value) {
        const next = value === 'dark' ? 'dark' : 'light';
        document.documentElement.dataset.theme = next;
        document.documentElement.style.colorScheme = next;
        native.setChrome({ dark: next === 'dark' });
      }

      async function toggleTheme() {
        const next = theme.value === 'dark' ? 'light' : 'dark';
        theme.value = next;
        applyTheme(next);
        try { window.localStorage?.setItem('ircapybara.theme', next); } catch (_) {}
        try {
          if (native.available()) await invoke('SetTheme', next);
        } catch (err) {
          showToast(errorText(err), 'error');
        }
      }

      async function openProtocolLog() {
        try {
          const trace = await invoke('GetProtocolTrace');
          if (Array.isArray(trace)) protocolTrace.value = trace;
        } catch (_) {}
        openModal('protocol');
        scrollProtocol();
      }

      async function copyProtocolLog() {
        const text = protocolTrace.value.map(e => `#${e.seq} ${formatTraceTime(e.time)} ${e.direction === 'out' ? 'OUT' : 'IN'} ${e.line}`).join('\n');
        await copyText(text, 'Protocol log copied.');
      }

      function loadNotificationPrefs() {
        try {
          const raw = window.localStorage?.getItem('ircapybara.notifications');
          if (!raw) return;
          const saved = JSON.parse(raw);
          if (typeof saved.enabled === 'boolean') notificationPrefs.enabled = saved.enabled;
          if (typeof saved.popup === 'boolean') notificationPrefs.popup = saved.popup;
          if (typeof saved.sound === 'boolean') notificationPrefs.sound = saved.sound;
          if (typeof saved.system === 'boolean') notificationPrefs.system = saved.system;
          // v0.4/v0.5 silently persisted the old false default before users
          // had a chance to choose a channel policy. Treat that legacy value as
          // unset; v2 and later preserve the user's explicit choice.
          if (saved.version >= 2 && typeof saved.allChannels === 'boolean') notificationPrefs.allChannels = saved.allChannels;
        } catch (_) {}
      }

      function saveNotificationPrefs() {
        try {
          window.localStorage?.setItem('ircapybara.notifications', JSON.stringify({
            version: 2,
            enabled: !!notificationPrefs.enabled,
            popup: !!notificationPrefs.popup,
            sound: !!notificationPrefs.sound,
            system: !!notificationPrefs.system,
            allChannels: !!notificationPrefs.allChannels,
          }));
        } catch (_) {}
      }


      function loadAppearancePrefs() {
        try {
          const raw = window.localStorage?.getItem('ircapybara.appearance');
          if (!raw) return;
          const saved = JSON.parse(raw);
          if (typeof saved.capybaraAvatars === 'boolean') appearancePrefs.capybaraAvatars = saved.capybaraAvatars;
        } catch (_) {}
      }

      function saveAppearancePrefs() {
        try {
          window.localStorage?.setItem('ircapybara.appearance', JSON.stringify({ capybaraAvatars: !!appearancePrefs.capybaraAvatars }));
        } catch (_) {}
      }

      function testNotificationSound() {
        unlockNotificationAudio();
        window.setTimeout(() => playNotificationSound(), 30);
      }

      function testInAppNotification() {
        showMessageToast({ name: 'general', id: '#general' }, { sender: 'capybara', text: 'This is how an in-app notification will look.' }, 7000);
        if (notificationPrefs.sound) testNotificationSound();
      }

      async function verifyCompletedDownloads() {
        if (!native.available()) return;
        const ids = Object.keys(downloadStates).filter(id => downloadStates[id]?.status === 'complete');
        for (const fileId of ids) {
          try {
            const exists = await invoke('DownloadedFileExists', fileId);
            if (!exists && downloadStates[fileId]?.status === 'complete') {
              downloadStates[fileId] = { status: 'available', fileId, total: downloadStates[fileId]?.total || 0, percent: 0 };
            }
          } catch (_) {}
        }
      }

      async function systemNotificationChanged() {
        if (!notificationPrefs.system || !notificationPrefs.enabled) return;
        try {
          const allowed = await invoke('PrepareSystemNotifications');
          if (!allowed) {
            notificationPrefs.system = false;
            showToast('System notifications are unavailable or were not allowed.', 'error', 4300);
          }
        } catch (err) {
          notificationPrefs.system = false;
          showToast(errorText(err), 'error');
        }
      }

      function bannerAction() {
        if (banner.value.actionKind === 'captcha') openModal('captcha');
        else if (banner.value.actionKind === 'identify') openModal('identify');
        else if (banner.value.actionKind === 'protocol') openProtocolLog();
      }

      async function bootstrap() {
        loadNotificationPrefs();
        loadAppearancePrefs();
        try {
          if (!native.available()) {
            applySnapshot(blankSnapshot(), false);
            try {
              const saved = window.localStorage?.getItem('ircapybara.theme');
              if (saved === 'light' || saved === 'dark') theme.value = saved;
            } catch (_) {}
            applyTheme(theme.value);
            return;
          }
          const result = await invoke('Bootstrap');
          if (result?.settings) {
            connection.host = result.settings.host || '';
            connection.port = result.settings.port || 6697;
            connection.nick = result.settings.nick || '';
            connection.displayName = result.settings.displayName || '';
            connection.serverLabel = result.settings.serverLabel || '';
            connection.autoJoinText = Array.isArray(result.settings.autoJoin) && result.settings.autoJoin.length ? result.settings.autoJoin.join(', ') : '#general';
            connection.pinnedCertificatePath = result.settings.pinnedCertificatePath || '';
            connection.pinServerCertificate = result.settings.tlsMode === 'pinned';
            connection.allowSelfSigned = result.settings.tlsMode === 'allow-self-signed';
            theme.value = result.settings.theme === 'dark' ? 'dark' : 'light';
            applyTheme(theme.value);
          }
          if (result?.platform) platform.value = result.platform;
          // The shell can outlive the page (the Android activity is recreated
          // while the Go core keeps its session), so restore transfer state too.
          for (const update of result?.transfers || []) {
            if (update && update.fileId) downloadStates[update.fileId] = { ...update };
          }
          applySnapshot(result?.snapshot || blankSnapshot(), false);
          if (isMobile.value && !isConnected.value && !connection.host) openModal('connect');
          if (notificationPrefs.enabled && notificationPrefs.system) {
            window.setTimeout(() => {
              invoke('PrepareSystemNotifications').then(allowed => {
                if (!allowed) notificationPrefs.system = false;
              }).catch(() => {});
            }, 450);
          }
        } catch (err) {
          showToast(`Startup failed: ${errorText(err)}`, 'error');
        }
      }

      watch(theme, applyTheme, { immediate: true });
      watch(platform, value => { document.documentElement.dataset.platform = value; }, { immediate: true });
      watch(mobilePane, value => { document.documentElement.dataset.pane = value; }, { immediate: true });
      // Returning to a conversation pane counts as reading it.
      watch(conversationVisible, visible => {
        if (visible && viewMode.value === 'conversation') {
          unread[activeId.value] = 0;
          scrollMessages(true);
        }
      });
      watch(notificationPrefs, saveNotificationPrefs, { deep: true });
      watch(appearancePrefs, saveAppearancePrefs, { deep: true });
      watch(mentionSuggestions, suggestions => {
        if (mentionIndex.value >= suggestions.length) mentionIndex.value = 0;
      });

      onMounted(async () => {
        await bootstrap();
        loadCapybaraAssets();
        eventCancels.push(native.on('ircapybara:state', next => applySnapshot(next, true)));
        eventCancels.push(native.on('ircapybara:transfer', handleTransferUpdate));
        eventCancels.push(native.on('ircapybara:trace', entry => {
          protocolTrace.value.push(entry);
          if (protocolTrace.value.length > 300) protocolTrace.value.splice(0, protocolTrace.value.length - 300);
          if (modal.value === 'protocol') scrollProtocol();
        }));
        native.onBack(goBack);
        eventCancels.push(window.IRCapybaraGestures.install({
          canSwipeBack: () => isPhone.value && mobilePane.value === 'chat' && !modal.value,
          onSwipeBack: () => { native.haptic('tap'); showList(); },
          canDismissSheet: () => isPhone.value && !!modal.value && modal.value !== 'captcha',
          onDismissSheet: closeModal,
          onLongPress: node => openMessageActions(messageFromNode(node)),
        }));
        // The on-screen keyboard shrinks the layout; keep the newest message in
        // view when the reader was already at the bottom.
        const keepLatestVisible = () => { if (!awayFromLatest.value) scrollMessages(true); };
        window.addEventListener('resize', keepLatestVisible);
        eventCancels.push(() => window.removeEventListener('resize', keepLatestVisible));
        const syncLayout = () => { isPhone.value = phoneLayout.matches; };
        phoneLayout.addEventListener('change', syncLayout);
        eventCancels.push(() => phoneLayout.removeEventListener('change', syncLayout));
        document.addEventListener('pointerdown', unlockNotificationAudio, { passive: true });
        const syncFocus = () => {
          const wasFocused = appFocused.value;
          appFocused.value = document.hasFocus() && !document.hidden;
          if (appFocused.value && !wasFocused) verifyCompletedDownloads();
        };
        window.addEventListener('focus', syncFocus);
        window.addEventListener('blur', syncFocus);
        document.addEventListener('visibilitychange', syncFocus);
        eventCancels.push(() => window.removeEventListener('focus', syncFocus));
        eventCancels.push(() => window.removeEventListener('blur', syncFocus));
        eventCancels.push(() => document.removeEventListener('visibilitychange', syncFocus));
        syncFocus();
        normalizeDraft();
        scrollMessages(true);
      });

      onBeforeUnmount(() => {
        for (const cancel of eventCancels) if (typeof cancel === 'function') cancel();
        eventCancels = [];
        document.removeEventListener('pointerdown', unlockNotificationAudio);
        try { audioContext?.close(); } catch (_) {}
      });

      return {
        snapshot, activeId, viewMode, platform, isMobile, mobilePane, actionMessage, awayFromLatest, arrivedBelow, theme, draft, modal, profileTarget, profileDetails, formError, busy, settingsTab, memberLoading, appFocused,
        identityNick, identityPassword, captchaAnswer, chatTarget, helpTopic,
        connection, messagesEl, messageInput, protocolEl, unread, downloadStates, toasts,
        protocolTrace, notificationPrefs, appearancePrefs,
        channelConversations, dmConversations, visibleConversations, selectedConversation,
        isConnected, canIdentify, canOpenChats, canCompose, canAttach, canSend,
        canRequestHistory, codePointCount, totalUnread, canReconnect, themeTitle,
        serverLabel, statusLabel, statusClass, identityLabel, headerTitle, headerSubtitle,
        headerIcon, emptyTitle, emptyCopy, composerPlaceholder, composerHint, banner, mentionSuggestions, mentionIndex,
        compactCount, avatarLetters, capybaraAvatar, kindLabel, conversationIcon, isOperator, openProfile, openProfileDM, profilePresence, profileLastSeen, formatTime, formatDateTime, formatTraceTime,
        showDaySeparator, formatDay, linkifyText, openExternalLink, formatBytes, fileBadge, lastPreview,
        openConversation, openOverview, showList, onMessagesScroll, jumpToLatest, firstLink, isOwnMessage,
        openMessageActions, copyActionMessage, copyCode, mentionActionSender, messageActionSender, closeConversation, openModal, closeModal, openSettings, openMembers, refreshMembers, connect, disconnect, reconnect, identify,
        solveCaptcha, openChat, requestHistory, refreshChannels, requestHelp, resetAI, normalizeDraft,
        sendMessage, pickAndUpload, downloadAttachment, restartAttachment, transferState, transferIsBusy, transferStatusLabel, transferButtonLabel, transferButtonIcon, pickTLSCertificate, toggleTheme,
        syncComposerCursor, handleComposerInput, handleComposerKeydown, selectMention,
        openProtocolLog, copyProtocolLog, systemNotificationChanged, testNotificationSound, testInAppNotification, bannerAction,
      };
    },
  }).mount('#app');
})();
