// Phase 4 Task 8 (PRD §66): records a short clip with MediaRecorder,
// posts it to POST /speech/transcribe (with the current session's id,
// so the recorded speech.transcribed event isn't session-less), and
// drops the returned text into the conversation pane's own input —
// #conversation-form's "text" field, the SAME field 送信 already
// posts through the unchanged /sessions/{id}/conversation route
// (conversationSay). It also stashes the response's event_id into the
// form's hidden #speech-event-id field, so that submission can tag
// the resulting turn as speech-sourced (code review Important I1's
// join key — see application/conversation.Service.Say's
// sourceEventID). This script never submits the form itself and never
// talks to the conversation route directly: pipeline reuse means the
// learner's spoken message goes through the exact same POST,
// application service, and events a typed one does — see
// internal/application/speech's package doc comment for why that
// split lives where it does.
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
  const sessionID = btn.dataset.sessionId;
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
      if (sessionID) form.append("session_id", sessionID);
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
      // Code review Important I1: thread the speech.transcribed
      // event's own id through the conversation form's hidden field,
      // so conversationSay can tag the resulting conversation.turn
      // event with the join key that ties it back to this exact
      // transcription — see application/conversation.Service.Say's
      // sourceEventID parameter.
      const eventIDField = document.getElementById("speech-event-id");
      if (eventIDField) eventIDField.value = json.event_id || "";
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
