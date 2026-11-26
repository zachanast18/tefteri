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
      scanSubmit.textContent = 'Scanning…';
      if (scanStatus) {
        scanStatus.textContent = 'Scanning receipt locally with Tesseract…';
      }
    });
  }

  // 2. Native <dialog> open/close buttons (e.g. mobile receipt zoom).
  document.querySelectorAll('[data-open-dialog]').forEach((btn) => {
    btn.addEventListener('click', () => {
      const id = btn.getAttribute('data-open-dialog');
      const dlg = document.getElementById(id);
      if (dlg && typeof dlg.showModal === 'function') {
        dlg.showModal();
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
});
