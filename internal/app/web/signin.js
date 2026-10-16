"use strict";
const form = document.querySelector("#signin");
const keyInput = document.querySelector("#admin-key");
const errorSlot = document.querySelector("#error");
const toggle = document.querySelector("#toggle-key");
const submit = document.querySelector("#authorize");
const submitLabel = document.querySelector("#submit-label");
let pending = false;

document.querySelector("#gateway-host").textContent = location.host;
function setError(message) {
  errorSlot.textContent = message;
  keyInput.setAttribute("aria-invalid", message ? "true" : "false");
}
function setPending(value) {
  pending = value;
  submit.disabled = value;
  keyInput.readOnly = value;
  toggle.disabled = value;
  form.setAttribute("aria-busy", String(value));
  submitLabel.textContent = value ? "正在验证…" : "进入控制台";
  document.querySelector("#submit-arrow").hidden = value;
  document.querySelector("#submit-spinner").hidden = !value;
}
toggle.addEventListener("click", () => {
  const show = keyInput.type === "password";
  keyInput.type = show ? "text" : "password";
  const label = show ? "隐藏密钥" : "显示密钥";
  toggle.setAttribute("aria-label", label);
  toggle.setAttribute("aria-pressed", String(show));
  toggle.title = label;
  toggle.querySelector("img").src = show ? "/auth-eye-off.svg" : "/auth-eye.svg";
  keyInput.focus();
});
keyInput.addEventListener("input", () => setError(""));
form.addEventListener("submit", async event => {
  event.preventDefault();
  if (pending) return;
  if (!keyInput.value.trim()) {
    setError("请输入管理员密钥");
    keyInput.focus();
    return;
  }
  setError("");
  setPending(true);
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 15000);
  try {
    const response = await fetch("/admin/auth/login", {
      method: "POST", headers: {"Content-Type":"application/json", "X-Hark-Admin":"1"},
      body: JSON.stringify({key:keyInput.value}), signal: controller.signal
    });
    const data = await response.json().catch(() => null);
    if (!response.ok) {
      const fallback = response.status === 429 ? "尝试次数较多，请稍后再试" : "授权失败，请重试";
      throw new Error(data?.error?.message || fallback);
    }
    if (!data?.ok) throw new Error("服务器响应异常，请重试");
    form.reset();
    keyInput.type = "password";
    location.replace("/");
  } catch (error) {
    const message = error.name === "AbortError" ? "连接超时，请重试" :
      error instanceof TypeError ? "连接失败，请检查网络后重试" : error.message;
    setError(message);
    keyInput.focus();
  } finally {
    clearTimeout(timeout);
    setPending(false);
  }
});
