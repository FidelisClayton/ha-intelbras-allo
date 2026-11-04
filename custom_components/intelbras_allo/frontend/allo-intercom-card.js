/* Home Assistant provides the authenticated connection and camera entity. */
class AlloIntercomCard extends HTMLElement {
  constructor() {
    super();
    this.attachShadow({ mode: 'open' });
    this._epoch = 0;
    this._hold = 0;
    this._status = 'Camera closed.';
    this.shadowRoot.innerHTML = `
      <style>
        :host { display: block; color: var(--primary-text-color); font-family: var(--ha-font-family, inherit); }
        ha-card { display: block; padding: 16px; }
        h2 { margin-block: 0 12px; font-size: var(--ha-font-size-xl, 20px); font-weight: var(--ha-font-weight-medium, 500); overflow-wrap: anywhere; }
        video { display: block; inline-size: 100%; aspect-ratio: 16 / 9; object-fit: contain; background: var(--secondary-background-color); border-radius: var(--ha-card-border-radius, 12px); }
        video[hidden] { display: none; }
        .controls { display: flex; flex-wrap: wrap; gap: 8px; margin-block-start: 12px; }
        button { flex: 1 1 auto; min-block-size: 44px; padding-block: 10px; padding-inline: 16px; border: 1px solid var(--divider-color); border-radius: var(--ha-border-radius-sm, 8px); background: var(--card-background-color); color: var(--primary-color); font: inherit; cursor: pointer; }
        button:active, button[aria-pressed="true"] { background: var(--primary-color); color: var(--text-primary-color); }
        button:focus-visible { outline: 2px solid var(--primary-color); outline-offset: 2px; }
        button:disabled { color: var(--disabled-text-color); cursor: default; }
        #talk { touch-action: none; user-select: none; -webkit-user-select: none; -webkit-touch-callout: none; }
        p { margin-block: 12px 0; overflow-wrap: anywhere; }
        .hint { color: var(--secondary-text-color); font-size: var(--ha-font-size-s, 14px); }
        @media (hover: hover) and (pointer: fine) { button:not(:disabled):hover { background: color-mix(in oklch, var(--primary-color) 12%, var(--card-background-color)); } }
      </style>
      <ha-card>
        <h2>Intercom</h2>
        <video aria-label="Intercom live camera" autoplay playsinline muted hidden></video>
        <div class="controls">
          <button id="open" type="button">Open camera</button>
          <button id="listen" type="button" disabled aria-pressed="false">Listen</button>
          <button id="talk" type="button" disabled aria-pressed="false">Hold to talk</button>
        </div>
        <p role="status" aria-live="polite"></p>
        <p class="hint">Hold to talk, then release to stop. With a keyboard, hold Space or Enter.</p>
      </ha-card>`;
    this._video = this.shadowRoot.querySelector('video');
    this._openButton = this.shadowRoot.querySelector('#open');
    this._listenButton = this.shadowRoot.querySelector('#listen');
    this._talkButton = this.shadowRoot.querySelector('#talk');
    this._openButton.addEventListener('click', () => this._peer ? this._close() : this._open());
    this._listenButton.addEventListener('click', () => this._listen());
    this._talkButton.addEventListener('pointerdown', (event) => {
      if (event.button !== 0 || this._holding) return;
      event.preventDefault();
      this._pointer = event.pointerId;
      try { this._talkButton.setPointerCapture(event.pointerId); } catch { /* Synthetic events have no active pointer. */ }
      this._startTalk();
    });
    this._talkButton.addEventListener('lostpointercapture', () => this._stopTalk());
    this._talkButton.addEventListener('keydown', (event) => {
      if (event.code !== 'Space' && event.code !== 'Enter') return;
      event.preventDefault();
      if (!event.repeat && !this._holding) this._startTalk();
    });
    this._talkButton.addEventListener('keyup', (event) => {
      if (event.code === 'Space' || event.code === 'Enter') { event.preventDefault(); this._stopTalk(); }
    });
    this._talkButton.addEventListener('blur', () => this._stopTalk());
    this._talkButton.addEventListener('contextmenu', event => event.preventDefault());
    this._release = event => {
      if (event.pointerId === this._pointer) this._stopTalk();
    };
    this._leave = () => this._close();
    this._blur = () => this._stopTalk();
    this._visibility = () => { if (document.hidden) this._close(); };
    this._disconnected = () => this._close('Home Assistant disconnected. Reconnect, then open the camera.');
    this._update();
  }

  setConfig(config) {
    if (!config?.entity?.startsWith('camera.')) throw new Error('Choose a camera entity.');
    if (this._config?.entity !== config.entity) this._close();
    this._config = { entity: config.entity };
    this._update();
  }

  set hass(hass) {
    const connection = hass?.connection;
    if (this._connection && this._connection !== connection) this._close('Home Assistant connection changed. Open the camera again.');
    this._connection?.removeEventListener('disconnected', this._disconnected);
    this._hass = hass;
    this._connection = connection;
    if (this.isConnected) connection?.addEventListener('disconnected', this._disconnected);
    this._update();
  }

  getCardSize() { return 5; }

  connectedCallback() {
    window.addEventListener('pointerup', this._release);
    window.addEventListener('pointercancel', this._release);
    window.addEventListener('blur', this._blur);
    window.addEventListener('pagehide', this._leave);
    window.addEventListener('popstate', this._leave);
    window.addEventListener('hashchange', this._leave);
    window.addEventListener('location-changed', this._leave);
    document.addEventListener('visibilitychange', this._visibility);
    this._connection?.addEventListener('disconnected', this._disconnected);
  }

  disconnectedCallback() {
    this._close();
    window.removeEventListener('pointerup', this._release);
    window.removeEventListener('pointercancel', this._release);
    window.removeEventListener('blur', this._blur);
    window.removeEventListener('pagehide', this._leave);
    window.removeEventListener('popstate', this._leave);
    window.removeEventListener('hashchange', this._leave);
    window.removeEventListener('location-changed', this._leave);
    document.removeEventListener('visibilitychange', this._visibility);
    this._connection?.removeEventListener('disconnected', this._disconnected);
  }

  _update() {
    this.shadowRoot.querySelector('h2').textContent = this._hass?.states?.[this._config?.entity]?.attributes?.friendly_name || 'Intercom';
    this.shadowRoot.querySelector('[role="status"]').textContent = this._status;
    this._openButton.textContent = this._opening ? 'Opening camera…' : this._peer ? 'Close camera' : 'Open camera';
    this._openButton.disabled = !!this._opening || !this._hass;
    this._listenButton.disabled = !this._session;
    this._talkButton.disabled = !this._session || (!this._holding && !!(this._talkPending || this._stopping));
    this._listenButton.textContent = this._video.muted ? 'Listen' : 'Mute';
    this._listenButton.setAttribute('aria-pressed', String(!this._video.muted));
    this._talkButton.setAttribute('aria-pressed', String(!!this._holding));
    this._video.hidden = !this._session;
  }

  async _open() {
    if (!this._hass || !this._config || this._peer) return;
    const epoch = ++this._epoch;
    const hass = this._hass;
    this._opening = true;
    this._status = 'Opening camera…';
    this._update();
    try {
      const peer = new RTCPeerConnection({ iceServers: [] });
      this._peer = peer;
      this._sender = peer.addTransceiver('audio', { direction: 'sendrecv' }).sender;
      peer.addTransceiver('video', { direction: 'recvonly' });
      const remote = new MediaStream();
      this._video.srcObject = remote;
      peer.addEventListener('track', event => {
        if (epoch !== this._epoch) return;
        remote.addTrack(event.track);
        this._video.play().catch(() => {
          if (epoch !== this._epoch) return;
          this._status = 'Camera playback was blocked. Press Listen to try again.';
          this._update();
        });
      });
      peer.addEventListener('connectionstatechange', () => {
        if (epoch === this._epoch && ['failed', 'disconnected', 'closed'].includes(peer.connectionState)) {
          this._close('Camera connection lost. Open the camera to reconnect.');
        }
      });
      await peer.setLocalDescription(await peer.createOffer());
      await this._gatherIce(peer);
      if (epoch !== this._epoch) return;
      const response = await hass.callWS({ type: 'intelbras_allo/open_media', entity_id: this._config.entity, offer: peer.localDescription.sdp });
      if (epoch !== this._epoch) {
        this._command(hass, 'close_media', response.session_id);
        return;
      }
      this._session = { id: response.session_id, hass };
      await peer.setRemoteDescription({ type: 'answer', sdp: response.answer });
      if (epoch !== this._epoch) return;
      this._opening = false;
      this._status = 'Camera open. Hold to talk.';
      this._update();
    } catch (error) {
      if (epoch !== this._epoch) return;
      const busy = /busy|in_use/.test(error?.code || '');
      this._close(busy ? 'Camera is in use. Close the other session, then try again.' : 'Could not open the camera. Check the connection, then try again.');
    }
  }

  _gatherIce(peer) {
    if (peer.iceGatheringState === 'complete') return Promise.resolve();
    return new Promise((resolve, reject) => {
      const finish = error => {
        clearTimeout(timer);
        peer.removeEventListener('icegatheringstatechange', check);
        this._cancelIce = undefined;
        error ? reject(error) : resolve();
      };
      const check = () => { if (peer.iceGatheringState === 'complete') finish(); };
      const timer = setTimeout(() => finish(new Error('ICE gathering timed out')), 10000);
      this._cancelIce = () => finish(new Error('Camera closed'));
      peer.addEventListener('icegatheringstatechange', check);
      check();
    });
  }

  async _listen() {
    if (!this._session) return;
    const epoch = this._epoch;
    this._video.muted = !this._video.muted;
    try {
      await this._video.play();
      if (epoch !== this._epoch) return;
      this._status = this._video.muted ? 'Camera audio muted.' : 'Listening to the camera.';
    } catch {
      if (epoch !== this._epoch) return;
      this._video.muted = true;
      this._status = 'Camera playback was blocked. Press Listen to try again.';
    }
    this._update();
  }

  async _startTalk() {
    if (!this._session || this._holding || this._talkPending || this._stopping) return;
    const hold = ++this._hold;
    const session = this._session;
    const sender = this._sender;
    const current = () => this._holding && hold === this._hold && this._session === session;
    this._holding = true;
    this._talkPending = true;
    this._status = 'Requesting microphone… Release to cancel.';
    this._update();
    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true, video: false });
      // Permission may finish after release, navigation, or another hold.
      for (const track of stream.getTracks()) track.enabled = false;
      if (!current()) { for (const track of stream.getTracks()) track.stop(); return; }
      this._mic = stream;
      const track = stream.getAudioTracks()[0];
      if (!track) throw new Error('No microphone track');
      await sender.replaceTrack(track);
      if (!current()) { if (sender.track === track) await sender.replaceTrack(null); return; }
      const result = await session.hass.callWS({ type: 'intelbras_allo/talk_start', session_id: session.id });
      if (!current()) { await this._stopRemote(session); return; }
      if (!result.talk_active) throw new Error('Talk did not start');
      track.enabled = true;
      this._scheduleKeepalive(hold, session);
      track.addEventListener?.('ended', () => { if (current()) this._stopTalk(); }, { once: true });
      this._status = 'Talking. Release to stop.';
      this._update();
    } catch (error) {
      if (!current()) return;
      this._stopTalk();
      this._status = error?.name === 'NotAllowedError' ? 'Microphone permission denied. Allow microphone access, then hold to talk.' : 'Could not start the microphone. Release, then hold to try again.';
      this._update();
    } finally {
      this._talkPending = false;
      if (this._status === 'Microphone off. Waiting for the previous request to finish.') this._status = 'Microphone off.';
      this._update();
    }
  }

  _scheduleKeepalive(hold, session, renewed = true) {
    const current = () => this._holding && hold === this._hold && this._session === session;
    const expire = () => {
      if (!current()) return;
      this._stopTalk(true);
      this._status = 'Hold to talk again';
      this._update();
    };
    if (renewed) {
      clearTimeout(this._keepaliveDeadline);
      // Two seconds until refresh, then at most three seconds for its response.
      this._keepaliveDeadline = setTimeout(expire, 5000);
    }
    this._keepaliveTimer = setTimeout(async () => {
      if (!current()) return;
      // A previous hold's request may still be pending on the same connection.
      if (this._keepalivePending) { this._scheduleKeepalive(hold, session, false); return; }
      this._keepalivePending = true;
      try {
        const result = await session.hass.callWS({ type: 'intelbras_allo/talk_keepalive', session_id: session.id });
        if (!result.talk_active) throw new Error('Talk lease inactive');
      } catch {
        expire();
      } finally {
        this._keepalivePending = false;
        if (current()) this._scheduleKeepalive(hold, session);
      }
    }, 2000);
  }

  _stopTalk(bestEffort = false) {
    clearTimeout(this._keepaliveTimer);
    clearTimeout(this._keepaliveDeadline);
    const wasHolding = this._holding;
    ++this._hold;
    this._holding = false;
    this._pointer = undefined;
    if (this._mic) {
      for (const track of this._mic.getTracks()) { track.enabled = false; track.stop(); }
      this._mic = undefined;
    }
    const session = this._session;
    if (wasHolding) {
      const sender = this._sender;
      if (sender) Promise.resolve(sender.replaceTrack(null)).catch(() => {
        if (!bestEffort && this._sender === sender) this._close('Microphone connection lost. Open the camera again.');
      });
      if (session) this._stopRemote(session, bestEffort);
      this._status = this._talkPending ? 'Microphone off. Waiting for the previous request to finish.' : 'Microphone off.';
    }
    this._update();
  }

  async _stopRemote(session, bestEffort = false) {
    this._stopping = (this._stopping || 0) + 1;
    try {
      await session.hass.callWS({ type: 'intelbras_allo/talk_stop', session_id: session.id });
    } catch {
      if (!bestEffort && this._session === session) this._close('Talk connection lost. Open the camera again.');
    } finally {
      --this._stopping;
      this._update();
    }
  }

  _command(hass, command, sessionId) {
    // Local microphone cleanup must never depend on a backend response.
    return hass.callWS({ type: `intelbras_allo/${command}`, session_id: sessionId }).catch(() => {});
  }

  _close(status = 'Camera closed.') {
    ++this._epoch;
    this._stopTalk();
    this._cancelIce?.();
    const session = this._session;
    this._session = undefined;
    const peer = this._peer;
    this._peer = undefined;
    this._sender = undefined;
    peer?.close();
    if (session) this._command(session.hass, 'close_media', session.id);
    this._video.srcObject = null;
    this._video.muted = true;
    this._opening = false;
    this._status = status;
    this._update();
  }
}

if (!customElements.get('allo-intercom-card')) customElements.define('allo-intercom-card', AlloIntercomCard);
window.customCards = window.customCards || [];
if (!window.customCards.some(card => card.type === 'allo-intercom-card')) {
  window.customCards.push({ type: 'allo-intercom-card', name: 'Allo intercom', description: 'Live camera, listening and hold-to-talk for an Allo camera entity.' });
}
