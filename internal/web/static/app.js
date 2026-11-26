document.addEventListener('DOMContentLoaded', () => {
  // 1. Scan page: live image preview and scan status announcement.
  const scanInput = document.getElementById('receipt-file-input');
  const previewWrap = document.getElementById('scan-preview-wrap');
  const previewImg = document.getElementById('scan-preview-img');
  const scanForm = document.getElementById('scan-form');
  const scanStatus = document.getElementById('scan-status');
  const scanSubmit = document.getElementById('scan-submit-btn');

  if (scanInput && previewWrap && previewImg) {
    scanInput.addEventListener('change', () => {
      const file = scanInput.files && scanInput.files[0];
      if (!file) {
        previewWrap.hidden = true;
        return;
      }
      const url = URL.createObjectURL(file);
      previewImg.src = url;
      previewWrap.hidden = false;
      if (scanStatus) {
        scanStatus.textContent = 'Photo selected. Ready to scan.';
      }
    });
  }

  if (scanForm && scanSubmit) {
    scanForm.addEventListener('submit', () => {
      scanSubmit.disabled = true;
      scanSubmit.textContent = 'Reading receipt…';
      if (scanStatus) {
        scanStatus.textContent = 'Reading receipt';
      }
    });
  }

  // 2. Native <dialog> open/close buttons with focus restoration.
  let lastDialogOpener = null;
  document.querySelectorAll('[data-open-dialog]').forEach((btn) => {
    btn.addEventListener('click', () => {
      const id = btn.getAttribute('data-open-dialog');
      const dlg = document.getElementById(id);
      if (dlg && typeof dlg.showModal === 'function') {
        lastDialogOpener = btn;
        dlg.showModal();
      }
    });
  });

  document.querySelectorAll('dialog').forEach((dlg) => {
    dlg.addEventListener('close', () => {
      if (lastDialogOpener && typeof lastDialogOpener.focus === 'function') {
        lastDialogOpener.focus();
        lastDialogOpener = null;
      }
    });
  });

  document.querySelectorAll('[data-close-dialog]').forEach((btn) => {
    btn.addEventListener('click', () => {
      const dlg = btn.closest('dialog');
      if (dlg && typeof dlg.close === 'function') {
        dlg.close();
      }
    });
  });

  // 3. Prevent accidental double submission on expense save form.
  document.querySelectorAll('form[data-prevent-double-submit]').forEach((form) => {
    form.addEventListener('submit', (e) => {
      if (form.dataset.submitted === 'true') {
        e.preventDefault();
        return;
      }
      form.dataset.submitted = 'true';
      const btn = form.querySelector('button[type="submit"]');
      if (btn) {
        btn.disabled = true;
      }
    });
  });

  // 4. Show "Remember this for this merchant" hint when category changes on expense form.
  const catSelect = document.getElementById('category_id');
  const rememberCheck = document.getElementById('remember_merchant');
  if (catSelect && rememberCheck) {
    const initial = catSelect.value;
    catSelect.addEventListener('change', () => {
      if (catSelect.value !== initial) {
        rememberCheck.checked = true;
      }
    });
  }

  // 5. Auto-submit household switcher without inline onchange handlers (strict CSP).
  document.querySelectorAll('[data-auto-submit]').forEach((el) => {
    el.addEventListener('change', () => {
      if (el.form) el.form.submit();
    });
  });

  // 6. WebAuthn Passkey helpers (registration & sign-in).
  const b64urlToBuf = (s) => {
    const pad = s.length % 4 === 0 ? '' : '='.repeat(4 - (s.length % 4));
    const bin = atob(s.replace(/-/g, '+').replace(/_/g, '/') + pad);
    const out = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out.buffer;
  };
  const bufToB64url = (buf) => {
    const bytes = new Uint8Array(buf);
    let bin = '';
    for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
    return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  };

  const regBtn = document.getElementById('register-passkey-btn');
  const regStatus = document.getElementById('passkey-status');
  if (regBtn && window.PublicKeyCredential) {
    regBtn.addEventListener('click', async () => {
      try {
        if (regStatus) regStatus.textContent = 'Starting passkey registration…';
        const csrf = regBtn.dataset.csrf || '';
        const res = await fetch('/webauthn/register/begin', {
          method: 'POST',
          headers: { 'X-CSRF-Token': csrf },
        });
        if (!res.ok) throw new Error(await res.text());
        const { challenge_id, options } = await res.json();
        const pk = options.publicKey;
        pk.challenge = b64urlToBuf(pk.challenge);
        pk.user.id = b64urlToBuf(pk.user.id);
        if (pk.excludeCredentials) {
          pk.excludeCredentials = pk.excludeCredentials.map((c) => ({ ...c, id: b64urlToBuf(c.id) }));
        }
        const cred = await navigator.credentials.create({ publicKey: pk });
        const payload = {
          id: cred.id,
          rawId: bufToB64url(cred.rawId),
          type: cred.type,
          response: {
            attestationObject: bufToB64url(cred.response.attestationObject),
            clientDataJSON: bufToB64url(cred.response.clientDataJSON),
          },
        };
        const label = encodeURIComponent(document.getElementById('passkey-label')?.value || 'Passkey');
        const fin = await fetch(`/webauthn/register/finish?challenge_id=${encodeURIComponent(challenge_id)}&label=${label}`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
          body: JSON.stringify(payload),
        });
        if (!fin.ok) throw new Error(await fin.text());
        window.location.reload();
      } catch (err) {
        if (regStatus) regStatus.textContent = 'Passkey registration failed: ' + err.message;
      }
    });
  }

  const loginBtn = document.getElementById('passkey-login-btn');
  const loginStatus = document.getElementById('passkey-login-status');
  if (loginBtn && window.PublicKeyCredential) {
    loginBtn.addEventListener('click', async () => {
      try {
        if (loginStatus) loginStatus.textContent = 'Waiting for passkey…';
        const csrf = loginBtn.dataset.csrf || '';
        const user = encodeURIComponent(document.getElementById('username')?.value || '');
        const res = await fetch(`/webauthn/login/begin?username=${user}`, {
          method: 'POST',
          headers: { 'X-CSRF-Token': csrf },
        });
        if (!res.ok) throw new Error(await res.text());
        const { challenge_id, options } = await res.json();
        const pk = options.publicKey;
        pk.challenge = b64urlToBuf(pk.challenge);
        if (pk.allowCredentials) {
          pk.allowCredentials = pk.allowCredentials.map((c) => ({ ...c, id: b64urlToBuf(c.id) }));
        }
        const assertion = await navigator.credentials.get({ publicKey: pk });
        const payload = {
          id: assertion.id,
          rawId: bufToB64url(assertion.rawId),
          type: assertion.type,
          response: {
            authenticatorData: bufToB64url(assertion.response.authenticatorData),
            clientDataJSON: bufToB64url(assertion.response.clientDataJSON),
            signature: bufToB64url(assertion.response.signature),
            userHandle: assertion.response.userHandle ? bufToB64url(assertion.response.userHandle) : '',
          },
        };
        const fin = await fetch(`/webauthn/login/finish?challenge_id=${encodeURIComponent(challenge_id)}`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
          body: JSON.stringify(payload),
        });
        if (!fin.ok) throw new Error(await fin.text());
        const data = await fin.json();
        window.location.href = data.redirect || '/';
      } catch (err) {
        if (loginStatus) loginStatus.textContent = 'Passkey sign-in failed: ' + err.message;
      }
    });
  }
});
