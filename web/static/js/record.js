// Phase 4 Task 8 (PRD §66): records a short clip with MediaRecorder,
// posts it to POST /speech/transcribe, and drops the returned text
// into the conversation pane's own input — #conversation-form's
// "text" field, the SAME field 送信 already posts through the
// unchanged /sessions/{id}/conversation route (conversationSay). This
// script never submits the form itself and never talks to the
// conversation route directly: pipeline reuse means the learner's
// spoken message goes through the exact same POST, application
// service, and events a typed one does — see internal/application/
// speech's package doc comment for why that split lives where it
// does.
//
// Progressively enhanced: if the browser has no MediaRecorder or
// getUserMedia, #record-btn is hidden entirely and typed input keeps
// working exactly as before this task existed.
window.jlp = window.jlp || {};

(function () {
  const btn = document.getElementById("record-btn");
  if (!btn) return; // no conversation pane on this page

  const status = document.getElementById("record-status");
  const supported =
    "MediaRecorder" in window &&
    navigator.mediaDevices &&
    typeof navigator.mediaDevices.getUserMedia === "function";
  if (!supported) {
    btn.hidden = true;
    return;
  }

  const transcribeURL = btn.dataset.transcribeUrl;
  const idleLabel = btn.textContent;
  const recordingLabel = "■ 停止";

  let mediaRecorder = null;
  let chunks = [];

  function setStatus(text) {
    if (!status) return;
    status.textContent = text;
    status.hidden = !text;
  }

  function setRecordingAppearance(recording) {
    btn.textContent = recording ? recordingLabel : idleLabel;
    btn.classList.toggle("btn--danger", recording);
    btn.classList.toggle("btn--secondary", !recording);
  }

  async function handleStop(stream) {
    stream.getTracks().forEach((track) => track.stop());
    setRecordingAppearance(false);
    btn.disabled = true;
    setStatus("書き起こし中…");
    try {
      const blob = new Blob(chunks, { type: mediaRecorder.mimeType || "audio/webm" });
      const form = new FormData();
      form.append("audio", blob, "clip.webm");
      const res = await fetch(transcribeURL, { method: "POST", body: form });
      if (!res.ok) {
        const body = await res.json().catch(() => ({}));
        throw new Error(body.error || `HTTP ${res.status}`);
      }
      const json = await res.json();
      const input = document.querySelector('#conversation-form input[name="text"]');
      if (input) {
        input.value = json.text || "";
        input.focus();
      }
      setStatus("");
    } catch (err) {
      setStatus("書き起こしに失敗しました: " + err.message);
    } finally {
      btn.disabled = false;
    }
  }

  btn.addEventListener("click", async () => {
    if (mediaRecorder && mediaRecorder.state === "recording") {
      mediaRecorder.stop();
      return;
    }

    setStatus("");
    let stream;
    try {
      stream = await navigator.mediaDevices.getUserMedia({ audio: true });
    } catch (err) {
      setStatus("マイクを使用できません: " + err.message);
      return;
    }

    chunks = [];
    mediaRecorder = new MediaRecorder(stream);
    mediaRecorder.addEventListener("dataavailable", (e) => {
      if (e.data && e.data.size > 0) chunks.push(e.data);
    });
    mediaRecorder.addEventListener("stop", () => handleStop(stream));
    mediaRecorder.start();
    setRecordingAppearance(true);
    setStatus("録音中…");
  });
})();
