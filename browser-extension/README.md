# Hark2API 账号导入助手

在打开本地管理页的同一个 Chrome / Edge 中安装一次：

1. 打开扩展管理页面（Chrome: chrome://extensions；Edge: edge://extensions）。
2. 开启「开发者模式」，选择「加载已解压的扩展程序」。
3. 选择本项目目录中的 `browser-extension` 文件夹。
4. 打开本机管理页 http://127.0.0.1:8789 并登录管理员；刷新页面，点击「导入账号」。界面会显示扩展已连接。
5. 生成登录链接，点击「打开授权页面」。Hark 在同一浏览器窗口的新标签页打开。
6. 正常登录 Hark。扩展检测有效会话后，网关验证 Cookie 并自动显示导入成功。

已经登录 Hark 时仍需从管理页发起一次导入；扩展不会在空闲时收集 Cookie。
首次安装后已打开的管理页需要刷新。隐身模式暂不支持。

Hark 官网自行启动 OAuth，处理 Google 等供应商回调和 state Cookie。
本地 bridge_token 只关联一次导入，15 分钟过期，成功后消费。它不是 Hark OAuth state。
扩展只接受 origins.js 中明确配置的管理页地址，Cookie 只发给本次发起导入的网关；
只发送 session 命名的 Cookie，不读取 Google Cookie。Cookie 和授权标识不进入 URL 或日志。
网关验证失败会显示错误；403 等错误不会假称导入成功。
远程部署请自行配置 HTTPS 和允许的网关来源，不要把服务器地址或密钥写入扩展源码。
已安装旧版扩展时，在扩展管理页点击「重新加载」，然后刷新管理页；服务器也提供 extension.zip 下载。
Cookie 仅存于网关内存，服务重启后需再次导入；扩展禁用或移除后不再执行导入。
