"use strict";
importScripts("origins.js");
const HARK="https://hark.com";
const busy=new Set();
const jobsKey="harkLoginJobs";
let mutation=Promise.resolve();
function mutate(fn) {
  const result=mutation.then(async()=>{const jobs=(await chrome.storage.session.get(jobsKey))[jobsKey]||{};const value=await fn(jobs);await chrome.storage.session.set({[jobsKey]:jobs});return value;});
  mutation=result.catch(()=>{});return result;
}
async function jobs(){await mutation;return (await chrome.storage.session.get(jobsKey))[jobsKey]||{};}
async function progress(job,message){try{await chrome.tabs.sendMessage(job.dashboard,{type:"login-progress",id:job.id,message});}catch{}}
async function remove(id){await mutate(data=>{delete data[id];});}
function validSender(sender){try{return sender.tab?.id!==undefined && !sender.tab.incognito && HARK_GATEWAYS.includes(new URL(sender.url).origin);}catch{return false;}}
function validateAttempt(a){
  return a && /^login_[a-f0-9]{48}$/.test(a.id) && /^bridge_[a-f0-9]{48}$/.test(a.bridge_token) && a.auth_url===HARK+"/login" && Date.parse(a.expires_at)>Date.now() && Date.parse(a.expires_at)<=Date.now()+16*60*1000;
}
chrome.runtime.onMessage.addListener((message,sender,reply)=>{
  if(!validSender(sender)){reply({ok:false,error:"请从本机 Hark2API 管理页发起导入"});return;}
  (async()=>{
    if(message.action==="ping")return {ok:true,version:chrome.runtime.getManifest().version};
    const a=message.attempt;
    if(message.action==="start"){
      if(!validateAttempt(a))throw new Error("导入请求已失效，请重新生成");
      const active=await jobs();
      if(Object.values(active).some(job=>job.dashboard===sender.tab.id && job.id!==a.id))throw new Error("请先完成或取消已有导入");
      if(active[a.id]){await chrome.tabs.update(active[a.id].tab,{active:true});return {ok:true};}
      // Chrome creates a normal tab in the very window containing the dashboard.
      // No browser launch, custom profile or standalone application popup is used.
      const tab=await chrome.tabs.create({url:HARK+"/login",windowId:sender.tab.windowId,active:true});
      const job={...a,origin:new URL(sender.url).origin,dashboard:sender.tab.id,tab:tab.id,store:sender.tab.incognito?"1":"0"};
      const stores=await chrome.cookies.getAllCookieStores();
      job.store=stores.find(store=>store.tabIds.includes(tab.id))?.id||job.store;
      await mutate(data=>{data[a.id]=job;});
      await chrome.alarms.create("hark-login-check",{periodInMinutes:0.5});
      setTimeout(()=>check(a.id),1000);
      return {ok:true};
    }
    const job=(await jobs())[a?.id];
    if(!job || job.dashboard!==sender.tab.id)return {ok:true};
    if(message.action==="cancel"){await remove(job.id);return {ok:true};}
    if(message.action==="retry"){void check(job.id);return {ok:true};}
    throw new Error("未知操作");
  })().then(reply,error=>reply({ok:false,error:error.message}));
  return true;
});

async function check(id){
  if(busy.has(id))return;
  busy.add(id);
  try{
    const job=(await jobs())[id];if(!job)return;
    if(Date.parse(job.expires_at)<=Date.now()){await progress(job,"导入请求已过期，请重新生成");await remove(id);return;}
    const tab=await chrome.tabs.get(job.tab);
    if(new URL(tab.url||"about:blank").origin!==HARK)return;
    // Hark's own page carries state/PKCE through OAuth and sends HttpOnly Cookie.
    const results=await chrome.scripting.executeScript({target:{tabId:job.tab},func:async()=>{
      if(location.origin!=="https://hark.com")return false;
      const response=await fetch("/api/auth/get-session",{credentials:"include",cache:"no-store"});
      if(!response.ok)return false;
      const data=await response.json();return Boolean(data?.session?.id && data?.user?.id);
    }});
    if(!results[0]?.result){await progress(job,"等待 Hark 登录完成…");return;}
    const all=await chrome.cookies.getAll({url:HARK+"/api/auth/get-session",storeId:job.store});
    const session=all.filter(c=>/(?:^|[._-])session(?:[._-]|$)/i.test(c.name) && !/[;\r\n]/.test(c.name+c.value));
    const cookie=session.map(c=>`${c.name}=${c.value}`).join("; ");
    if(!cookie)throw new Error("登录已完成，但未找到 Hark 会话 Cookie，请检查账号状态");
    // Check cancellation again after browser/network operations.
    if(!(await jobs())[id])return;
    if(!HARK_GATEWAYS.includes(job.origin))throw new Error("网关地址已失效，请重新发起导入");
    const response=await fetch(job.origin+"/bridge/login",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({id:job.id,token:job.bridge_token,cookie}),signal:AbortSignal.timeout(25000)});
    const data=await response.json();
    if(!response.ok){if(response.status===410){await remove(id);return;}throw new Error(data.error?.message||"本地导入失败");}
    await remove(id);
    await progress(job,"登录会话已验证，账号已导入");
    try{await chrome.tabs.update(job.dashboard,{active:true});}catch{}
  }catch(error){const job=(await jobs())[id];if(job)await progress(job,error.message||"会话读取失败，请重试");}
  finally{busy.delete(id);if((await jobs())[id])setTimeout(()=>check(id),3000);}
}
chrome.tabs.onUpdated.addListener((tabId,change)=>{if(change.status==="complete")void jobs().then(data=>{for(const job of Object.values(data))if(job.tab===tabId)void check(job.id);});});
chrome.tabs.onRemoved.addListener(tabId=>{void jobs().then(async data=>{for(const job of Object.values(data))if(job.tab===tabId || job.dashboard===tabId){await progress(job,"登录页面已关闭，请重新生成导入请求");await remove(job.id);}});});
chrome.alarms.onAlarm.addListener(alarm=>{if(alarm.name==="hark-login-check")void jobs().then(async data=>{if(!Object.keys(data).length){await chrome.alarms.clear(alarm.name);return;}for(const id of Object.keys(data))void check(id);});});
