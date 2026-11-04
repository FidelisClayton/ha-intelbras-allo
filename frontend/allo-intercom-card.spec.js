import { test, expect } from '@playwright/test';
import { fileURLToPath } from 'node:url';

const cardPath = fileURLToPath(new URL('../custom_components/intelbras_allo/frontend/allo-intercom-card.js', import.meta.url));

async function mount(page, options = {}) {
  await page.setContent('<main></main>');
  await page.evaluate((options) => {
    window.calls = [];
    window.tracks = [];
    window.playCalls = [];
    window.peers = [];
    window.connection = new EventTarget();
    window.micRequested = 0;
    HTMLMediaElement.prototype.play = function () {
      window.playCalls.push(this.muted);
      return options.blockPlayback ? Promise.reject(new Error('blocked')) : Promise.resolve();
    };
    const mic = () => {
      const track = { kind: 'audio', enabled: true, stopped: false, stop() { this.stopped = true; } };
      window.tracks.push(track);
      return { getTracks: () => [track], getAudioTracks: () => [track] };
    };
    Object.defineProperty(navigator, 'mediaDevices', { configurable: true, value: {
      getUserMedia: () => {
        window.micRequested++;
        if (options.denyPermissionOnce && window.micRequested === 1) return Promise.reject(new DOMException('Permission denied', 'NotAllowedError'));
        if (options.deferPermission) return new Promise(resolve => { window.grantMic = () => resolve(mic()); });
        return Promise.resolve(mic());
      },
    } });
    window.RTCPeerConnection = class extends EventTarget {
      constructor(config) {
        super(); this.config = config; this.iceGatheringState = options.deferIce ? 'gathering' : 'complete'; this.connectionState = 'new';
        this.transceivers = []; window.peers.push(this);
      }
      addTransceiver(kind, init) {
        const sender = { track: null, replaceTrack(track) {
          if (track && options.deferReplace) return new Promise(resolve => { window.finishReplace = () => { this.track = track; resolve(); }; });
          this.track = track; return Promise.resolve();
        } };
        this.transceivers.push({ kind, ...init, sender }); return { sender };
      }
      createOffer() { return Promise.resolve({ type: 'offer', sdp: 'synthetic full SDP offer' }); }
      setLocalDescription(offer) { this.localDescription = offer; return Promise.resolve(); }
      setRemoteDescription(answer) { this.answer = answer; return Promise.resolve(); }
      close() { this.closed = true; }
    };
    window.hass = {
      connection: window.connection,
      states: { 'camera.front_door': { attributes: { friendly_name: 'Front door' } } },
      callWS: async (request) => {
        window.calls.push(request);
        if (request.type.endsWith('/open_media')) {
          if (options.busy) throw { code: 'session_busy', message: 'secret internal detail' };
          if (options.deferOpen) return new Promise(resolve => { window.finishOpen = () => resolve({ session_id: 'session', answer: 'synthetic answer' }); });
          return { session_id: 'session', answer: 'synthetic answer' };
        }
        if (request.type.endsWith('/talk_start') && options.deferStart) {
          return new Promise(resolve => { window.finishStart = () => resolve({ talk_active: true }); });
        }
        if (request.type.endsWith('/talk_keepalive')) {
          if (options.deferKeepalive) return new Promise((resolve, reject) => { window.finishKeepalive = resolve; window.failKeepalive = () => reject({ code: 'talk_inactive', message: 'private backend details' }); });
          if (options.failKeepalive) throw { code: 'talk_inactive', message: 'private backend details' };
          return { talk_active: !options.inactiveKeepalive };
        }
        if (request.type.endsWith('/talk_start') && options.failStart) throw new Error('private backend details');
        if (request.type.endsWith('/talk_stop') && options.failStop) throw new Error('private backend details');
        if (request.type.endsWith('/talk_stop') && options.deferStop) return new Promise(resolve => { window.finishStop = () => resolve({ talk_active: false }); });
        return { talk_active: request.type.endsWith('/talk_start'), closed: true };
      },
    };
  }, options);
  await page.addScriptTag({ path: cardPath });
  await page.evaluate(() => {
    const card = document.createElement('allo-intercom-card');
    card.setConfig({ entity: 'camera.front_door' }); card.hass = window.hass;
    document.querySelector('main').append(card);
  });
}

async function open(page) {
  await page.getByRole('button', { name: 'Open camera', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Hold to talk', exact: true })).toBeEnabled();
}

async function expectStopped(page) {
  await expect.poll(() => page.evaluate(() => window.tracks.every(t => !t.enabled && t.stopped))).toBe(true);
  await expect.poll(() => page.evaluate(() => window.peers[0].transceivers.find(t => t.kind === 'audio').sender.track)).toBeNull();
}

for (const width of [1280, 320]) {
  test(`controls fit ${width}px and negotiate without microphone or external ICE`, async ({ page }) => {
    await page.setViewportSize({ width, height: 800 });
    await mount(page); await open(page);
    await expect(page.getByRole('heading', { name: 'Front door' })).toBeVisible();
    for (const name of ['Close camera', 'Listen', 'Hold to talk']) {
      const button = page.getByRole('button', { name, exact: true });
      await expect(button).toBeVisible();
      const bounds = await button.boundingBox();
      expect(bounds.x).toBeGreaterThanOrEqual(0); expect(bounds.x + bounds.width).toBeLessThanOrEqual(width);
    }
    expect(await page.evaluate(() => window.micRequested)).toBe(0);
    expect(await page.evaluate(() => window.peers[0].config)).toEqual({ iceServers: [] });
    expect(await page.evaluate(() => window.peers[0].transceivers.map(({ kind, direction }) => ({ kind, direction })))).toEqual([
      { kind: 'audio', direction: 'sendrecv' }, { kind: 'video', direction: 'recvonly' },
    ]);
    const video = page.locator('video');
    await expect(video).toHaveAttribute('playsinline', ''); await expect(video).toHaveAttribute('autoplay', '');
    await page.getByRole('button', { name: 'Listen', exact: true }).click();
    expect(await video.evaluate(v => v.muted)).toBe(false);
    await page.getByRole('button', { name: 'Mute', exact: true }).click();
    expect(await video.evaluate(v => v.muted)).toBe(true);
  });
}

test('permission resolved after release stops every returned track without starting talk', async ({ page }) => {
  await mount(page, { deferPermission: true }); await open(page);
  await page.getByRole('button', { name: 'Hold to talk', exact: true }).dispatchEvent('pointerdown', { pointerId: 1, button: 0 });
  await page.dispatchEvent('body', 'pointerup', { pointerId: 1 });
  await page.evaluate(() => window.grantMic());
  await expectStopped(page);
  expect(await page.evaluate(() => window.calls.filter(c => c.type.endsWith('/talk_start')).length)).toBe(0);
});

for (const ending of ['pointerup', 'pointercancel', 'lostpointercapture', 'keyboard', 'enter', 'blur', 'hidden', 'pagehide', 'location-changed', 'disconnect', 'rtc', 'websocket']) {
  test(`holding stops immediately on ${ending}`, async ({ page }) => {
    await page.clock.install({ time: new Date('2026-01-01T00:00:00Z') });
    await page.clock.pauseAt(new Date('2026-01-01T00:00:01Z'));
    await mount(page); await open(page);
    const talk = page.getByRole('button', { name: 'Hold to talk', exact: true });
    if (ending === 'keyboard' || ending === 'enter') { await talk.focus(); await page.keyboard.down(ending === 'enter' ? 'Enter' : 'Space'); }
    else await talk.dispatchEvent('pointerdown', { pointerId: 1, button: 0 });
    await expect.poll(() => page.evaluate(() => window.tracks[0]?.enabled)).toBe(true);
    if (ending === 'keyboard' || ending === 'enter') await page.keyboard.up(ending === 'enter' ? 'Enter' : 'Space');
    else if (ending === 'lostpointercapture') await talk.dispatchEvent(ending, { pointerId: 1 });
    else if (ending === 'disconnect') await page.evaluate(() => document.querySelector('allo-intercom-card').remove());
    else if (ending === 'rtc') await page.evaluate(() => { window.peers[0].connectionState = 'failed'; window.peers[0].dispatchEvent(new Event('connectionstatechange')); });
    else if (ending === 'websocket') await page.evaluate(() => window.connection.dispatchEvent(new Event('disconnected')));
    else if (ending === 'hidden') await page.evaluate(() => { Object.defineProperty(document, 'hidden', { configurable: true, value: true }); document.dispatchEvent(new Event('visibilitychange')); });
    else if (ending === 'blur' || ending === 'pagehide' || ending === 'location-changed') await page.evaluate(ending => window.dispatchEvent(new Event(ending)), ending);
    else await page.dispatchEvent('body', ending, { pointerId: 1 });
    await expectStopped(page);
    await expect.poll(() => page.evaluate(() => window.calls.some(c => c.type.endsWith('/talk_stop')))).toBe(true);
    await page.clock.runFor(10000);
    expect(await keepaliveCount(page)).toBe(0);
  });
}

test('talk start finishing after release sends stop and never enables microphone', async ({ page }) => {
  await mount(page, { deferStart: true }); await open(page);
  await page.getByRole('button', { name: 'Hold to talk', exact: true }).dispatchEvent('pointerdown', { pointerId: 1, button: 0 });
  await expect.poll(() => page.evaluate(() => typeof window.finishStart)).toBe('function');
  await page.dispatchEvent('body', 'pointerup', { pointerId: 1 });
  await expectStopped(page);
  const stops = await page.evaluate(() => window.calls.filter(c => c.type.endsWith('/talk_stop')).length);
  await page.evaluate(() => window.finishStart());
  await expect.poll(() => page.evaluate(() => window.calls.filter(c => c.type.endsWith('/talk_stop')).length)).toBeGreaterThan(stops);
  await expectStopped(page);
});

test('backend busy is actionable and hides private errors', async ({ page }) => {
  await mount(page, { busy: true });
  await page.getByRole('button', { name: 'Open camera', exact: true }).click();
  await expect(page.getByRole('status')).toContainText('in use');
  await expect(page.getByRole('status')).not.toContainText('secret');
  await expect(page.getByRole('button', { name: 'Open camera', exact: true })).toBeEnabled();
});

test('talk errors stop capture and offer retry', async ({ page }) => {
  await page.clock.install({ time: new Date('2026-01-01T00:00:00Z') });
  await page.clock.pauseAt(new Date('2026-01-01T00:00:01Z'));
  await mount(page, { failStart: true }); await open(page);
  await page.getByRole('button', { name: 'Hold to talk', exact: true }).dispatchEvent('pointerdown', { pointerId: 1, button: 0 });
  await expect(page.getByRole('status')).toContainText('Could not start');
  await expectStopped(page);
  await expect(page.getByRole('status')).not.toContainText('private');
  await page.clock.runFor(10000);
  expect(await keepaliveCount(page)).toBe(0);
});

test('autoplay failure offers explicit Listen control', async ({ page }) => {
  await mount(page, { blockPlayback: true }); await open(page);
  await page.getByRole('button', { name: 'Listen', exact: true }).click();
  await expect(page.getByRole('status')).toContainText('playback');
  expect(await page.locator('video').evaluate(v => v.muted)).toBe(true);
});

test('late open response is closed after card removal', async ({ page }) => {
  await mount(page, { deferOpen: true });
  await page.getByRole('button', { name: 'Open camera', exact: true }).click();
  await expect.poll(() => page.evaluate(() => typeof window.finishOpen)).toBe('function');
  await page.evaluate(() => { document.querySelector('allo-intercom-card').remove(); window.finishOpen(); });
  await expect.poll(() => page.evaluate(() => window.calls.some(c => c.type.endsWith('/close_media')))).toBe(true);
  expect(await page.evaluate(() => window.peers[0].closed)).toBe(true);
});

test('a new hold waits for the previous pending start to settle', async ({ page }) => {
  await mount(page, { deferStart: true }); await open(page);
  const talk = page.getByRole('button', { name: 'Hold to talk', exact: true });
  await talk.dispatchEvent('pointerdown', { pointerId: 1, button: 0 });
  await expect.poll(() => page.evaluate(() => typeof window.finishStart)).toBe('function');
  await page.dispatchEvent('body', 'pointerup', { pointerId: 1 });
  await talk.dispatchEvent('pointerdown', { pointerId: 2, button: 0 });
  expect(await page.evaluate(() => window.micRequested)).toBe(1);
  const stops = await page.evaluate(() => window.calls.filter(c => c.type.endsWith('/talk_stop')).length);
  await page.evaluate(() => window.finishStart());
  await expect.poll(() => page.evaluate(() => window.calls.filter(c => c.type.endsWith('/talk_stop')).length)).toBeGreaterThan(stops);
  await expect(talk).toBeEnabled();
  await expectStopped(page);
});

for (const option of ['deferStop', 'failStop']) {
  test(`local microphone stops even when backend stop ${option}`, async ({ page }) => {
    await mount(page, { [option]: true }); await open(page);
    const talk = page.getByRole('button', { name: 'Hold to talk', exact: true });
    await talk.dispatchEvent('pointerdown', { pointerId: 1, button: 0 });
    await expect.poll(() => page.evaluate(() => window.tracks[0]?.enabled)).toBe(true);
    await page.dispatchEvent('body', 'pointerup', { pointerId: 1 });
    await expectStopped(page);
    if (option === 'deferStop') {
      await expect(talk).toBeDisabled();
      await page.evaluate(() => window.finishStop());
      await expect(talk).toBeEnabled();
    } else {
      await expect(page.getByRole('button', { name: 'Open camera', exact: true })).toBeEnabled();
      await expect(page.getByRole('status')).toContainText('Talk connection lost');
    }
  });
}

test('real mouse release outside the button stops captured hold', async ({ page }) => {
  await mount(page); await open(page);
  const talk = page.getByRole('button', { name: 'Hold to talk', exact: true });
  await talk.hover(); await page.mouse.down();
  await expect.poll(() => page.evaluate(() => window.tracks[0]?.enabled)).toBe(true);
  await page.mouse.move(0, 0); await page.mouse.up();
  await expectStopped(page);
});

test('offer is sent only after all ICE candidates have gathered', async ({ page }) => {
  await mount(page, { deferIce: true });
  await page.getByRole('button', { name: 'Open camera', exact: true }).click();
  expect(await page.evaluate(() => window.calls)).toEqual([]);
  await page.evaluate(() => {
    const peer = window.peers[0];
    peer.localDescription.sdp = 'synthetic complete SDP with candidates';
    peer.iceGatheringState = 'complete';
    peer.dispatchEvent(new Event('icegatheringstatechange'));
  });
  await expect(page.getByRole('button', { name: 'Hold to talk', exact: true })).toBeEnabled();
  expect(await page.evaluate(() => window.calls[0].offer)).toBe('synthetic complete SDP with candidates');
});

test('release before replaceTrack completes stops capture and detaches the late track', async ({ page }) => {
  await mount(page, { deferReplace: true }); await open(page);
  await page.getByRole('button', { name: 'Hold to talk', exact: true }).dispatchEvent('pointerdown', { pointerId: 1, button: 0 });
  await expect.poll(() => page.evaluate(() => typeof window.finishReplace)).toBe('function');
  await page.dispatchEvent('body', 'pointerup', { pointerId: 1 });
  await expectStopped(page);
  await page.evaluate(() => window.finishReplace());
  await expectStopped(page);
  expect(await page.evaluate(() => window.calls.some(c => c.type.endsWith('/talk_start')))).toBe(false);
});

test('microphone permission denial preserves camera and listening, then permits another hold', async ({ page }) => {
  await mount(page, { denyPermissionOnce: true }); await open(page);
  const talk = page.getByRole('button', { name: 'Hold to talk', exact: true });
  await talk.dispatchEvent('pointerdown', { pointerId: 1, button: 0 });
  await expect(page.getByRole('status')).toContainText('Microphone permission denied');
  expect(await page.evaluate(() => window.calls.some(c => c.type.endsWith('/talk_start')))).toBe(false);
  expect(await page.evaluate(() => window.tracks)).toEqual([]);
  await expectStopped(page);
  await expect(page.getByRole('button', { name: 'Close camera', exact: true })).toBeEnabled();
  await page.getByRole('button', { name: 'Listen', exact: true }).click();
  expect(await page.locator('video').evaluate(v => v.muted)).toBe(false);
  await expect(talk).toBeEnabled();
  await talk.dispatchEvent('pointerdown', { pointerId: 2, button: 0 });
  await expect.poll(() => page.evaluate(() => window.tracks[0]?.enabled)).toBe(true);
  await page.dispatchEvent('body', 'pointerup', { pointerId: 2 });
  await expectStopped(page);
});

const keepaliveCount = page => page.evaluate(() => window.calls.filter(c => c.type.endsWith('/talk_keepalive')).length);

async function holdWithClock(page, options = {}) {
  await page.clock.install({ time: new Date('2026-01-01T00:00:00Z') });
  await page.clock.pauseAt(new Date('2026-01-01T00:00:01Z'));
  await mount(page, options); await open(page);
  if (options.listen) await page.getByRole('button', { name: 'Listen', exact: true }).click();
  await page.getByRole('button', { name: 'Hold to talk', exact: true }).dispatchEvent('pointerdown', { pointerId: 1, button: 0 });
  await expect.poll(() => page.evaluate(() => window.tracks[0]?.enabled)).toBe(true);
}

test('a held microphone renews every two seconds beyond the five second lease, then release cancels refresh', async ({ page }) => {
  await holdWithClock(page);
  await page.clock.runFor(1999);
  expect(await keepaliveCount(page)).toBe(0);
  await page.clock.runFor(1);
  expect(await keepaliveCount(page)).toBe(1);
  await page.clock.runFor(4000);
  expect(await keepaliveCount(page)).toBe(3);
  expect(await page.evaluate(() => window.tracks[0].enabled && !window.tracks[0].stopped)).toBe(true);
  expect(await page.evaluate(() => window.calls.some(c => c.type.endsWith('/talk_stop')))).toBe(false);
  expect(await page.evaluate(() => window.calls.filter(c => c.type.endsWith('/talk_keepalive')).every(c => c.session_id === 'session'))).toBe(true);
  await page.dispatchEvent('body', 'pointerup', { pointerId: 1 });
  await expectStopped(page);
  await page.clock.runFor(10000);
  expect(await keepaliveCount(page)).toBe(3);
});

for (const options of [{ failKeepalive: true }, { inactiveKeepalive: true }]) {
  test(`lease failure stops local capture and preserves receive ${JSON.stringify(options)}`, async ({ page }) => {
    await holdWithClock(page, { ...options, failStop: true, listen: true });
    await page.clock.runFor(2000);
    await expectStopped(page);
    await expect(page.getByRole('status')).toHaveText('Hold to talk again');
    await expect(page.getByRole('button', { name: 'Close camera', exact: true })).toBeEnabled();
    await expect(page.getByRole('button', { name: 'Mute', exact: true })).toBeEnabled();
    expect(await page.evaluate(() => window.peers[0].closed)).not.toBe(true);
    expect(await page.evaluate(() => window.calls.some(c => c.type.endsWith('/talk_stop')))).toBe(true);
    await page.clock.runFor(10000);
    expect(await keepaliveCount(page)).toBe(1);
  });
}

for (const result of ['success', 'failure']) {
  test(`late old lease ${result} cannot stop a new hold`, async ({ page }) => {
    await holdWithClock(page, { deferKeepalive: true });
    await page.clock.runFor(2000);
    expect(await keepaliveCount(page)).toBe(1);
    await page.dispatchEvent('body', 'pointerup', { pointerId: 1 });
    await expectStopped(page);
    const talk = page.getByRole('button', { name: 'Hold to talk', exact: true });
    await expect(talk).toBeEnabled();
    await talk.dispatchEvent('pointerdown', { pointerId: 2, button: 0 });
    await expect.poll(() => page.evaluate(() => window.tracks[1]?.enabled)).toBe(true);
    const stops = await page.evaluate(() => window.calls.filter(c => c.type.endsWith('/talk_stop')).length);
    await page.clock.runFor(2000);
    expect(await keepaliveCount(page)).toBe(1);
    await page.evaluate(result => result === 'failure' ? window.failKeepalive() : window.finishKeepalive({ talk_active: true }), result);
    expect(await page.evaluate(() => window.tracks[1].enabled && !window.tracks[1].stopped)).toBe(true);
    expect(await page.evaluate(() => window.calls.filter(c => c.type.endsWith('/talk_stop')).length)).toBe(stops);
    await expect(talk).toHaveAttribute('aria-pressed', 'true');
    await page.clock.runFor(2000);
    expect(await keepaliveCount(page)).toBe(2);
    await page.dispatchEvent('body', 'pointerup', { pointerId: 2 });
    await expectStopped(page);
  });
}

test('a hung keepalive stays bounded and release stops capture without its response', async ({ page }) => {
  await holdWithClock(page, { deferKeepalive: true });
  await page.clock.runFor(2000);
  expect(await keepaliveCount(page)).toBe(1);
  await page.clock.runFor(2000);
  expect(await keepaliveCount(page)).toBe(1);
  await page.dispatchEvent('body', 'pointerup', { pointerId: 1 });
  await expectStopped(page);
  await page.clock.runFor(10000);
  expect(await keepaliveCount(page)).toBe(1);
  await page.evaluate(() => window.finishKeepalive({ talk_active: true }));
  await expectStopped(page);
  await page.clock.runFor(4000);
  expect(await keepaliveCount(page)).toBe(1);
});

test('refresh waits for start acknowledgement and never starts after a late acknowledgement', async ({ page }) => {
  await page.clock.install({ time: new Date('2026-01-01T00:00:00Z') });
  await page.clock.pauseAt(new Date('2026-01-01T00:00:01Z'));
  await mount(page, { deferStart: true }); await open(page);
  await page.clock.runFor(6000);
  expect(await keepaliveCount(page)).toBe(0);
  await page.getByRole('button', { name: 'Hold to talk', exact: true }).dispatchEvent('pointerdown', { pointerId: 1, button: 0 });
  await expect.poll(() => page.evaluate(() => typeof window.finishStart)).toBe('function');
  await page.clock.runFor(6000);
  expect(await keepaliveCount(page)).toBe(0);
  await page.dispatchEvent('body', 'pointerup', { pointerId: 1 });
  await page.evaluate(() => window.finishStart());
  await page.clock.runFor(6000);
  expect(await keepaliveCount(page)).toBe(0);
  await expectStopped(page);
});

test('a hung refresh stops local capture at its response deadline without another request', async ({ page }) => {
  await holdWithClock(page, { deferKeepalive: true, listen: true, failStop: true });
  await page.clock.runFor(2000);
  expect(await keepaliveCount(page)).toBe(1);
  await page.clock.runFor(2999);
  expect(await page.evaluate(() => window.tracks[0].enabled && !window.tracks[0].stopped)).toBe(true);
  await page.clock.runFor(1);
  await expectStopped(page);
  await expect(page.getByRole('status')).toHaveText('Hold to talk again');
  await expect(page.getByRole('button', { name: 'Close camera', exact: true })).toBeEnabled();
  await expect(page.getByRole('button', { name: 'Mute', exact: true })).toBeEnabled();
  expect(await page.evaluate(() => window.peers[0].closed)).not.toBe(true);
  await page.clock.runFor(20000);
  expect(await keepaliveCount(page)).toBe(1);
  const talk = page.getByRole('button', { name: 'Hold to talk', exact: true });
  await talk.dispatchEvent('pointerdown', { pointerId: 2, button: 0 });
  await expect.poll(() => page.evaluate(() => window.tracks[1]?.enabled)).toBe(true);
  await page.clock.runFor(2000);
  expect(await keepaliveCount(page)).toBe(1);
  await page.evaluate(() => window.finishKeepalive({ talk_active: true }));
  expect(await page.evaluate(() => window.tracks[1].enabled && !window.tracks[1].stopped)).toBe(true);
  await page.clock.runFor(2000);
  expect(await keepaliveCount(page)).toBe(2);
  await page.dispatchEvent('body', 'pointerup', { pointerId: 2 });
  await expectStopped(page);
});
