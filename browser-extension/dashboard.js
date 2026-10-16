"use strict";
// This bridge runs only on the exact local dashboard origin. It never sends a
// Cookie back into the DOM; the worker sends it straight to the local API.
window.addEventListener("message", async event => {
  if(event.source!==window || event.origin!==location.origin || !HARK_GATEWAYS.includes(event.origin)) return;
  const data=event.data;
  if(data?.channel!=="hark2api-request" || !["ping","start","cancel","retry"].includes(data.action) || typeof data.requestId!=="string") return;
  let reply;
  try { reply=await chrome.runtime.sendMessage({action:data.action, attempt:data.attempt}); }
  catch { reply={ok:false,error:"扩展连接已失效，请刷新管理页面"}; }
  window.postMessage({channel:"hark2api-response",requestId:data.requestId,...reply},event.origin);
});
chrome.runtime.onMessage.addListener(message => {
  if(message.type==="login-progress") window.postMessage({channel:"hark2api-progress",id:message.id,message:message.message},location.origin);
});
