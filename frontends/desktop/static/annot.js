// GenericAgent 桌面版 —— 开发脚手架(标注模式)。纯 dev 工具，不属于产品本体。
// 注释/区域说明/悬浮按钮全部由本文件运行时注入，产品 HTML 里没有任何标注痕迹。
// 删掉 index.html 里 annot.css + annot.js 两行 include 即可彻底移除。
(function () {
  'use strict';

  // 区域块标签：选择器 -> 说明
  const REGIONS = [
    ['.sidebar', '① 侧边栏（美术重点 · 老师要求#1）'],
    ['.topbar', '顶栏 · 仅运行状态'],
    ['.page[data-page="chat"]', '聊天页 · 消息流 GET /session/{sid}/messages，WS 通知新内容'],
    ['.composer', '② 输入区 · 发送=POST /session/{sid}/prompt'],
    ['.page[data-page="services"]', '③ 后台服务（消息通道 + 状态面板，融 hub.pyw）'],
    ['.page[data-page="collab"]', '④ subagent / Hive 多体执行监控'],
    ['.page[data-page="token"]', '④ token 记录与成本'],
    ['.rightpanel', '④ 会话管理 · GET /sessions'],
  ];

  // 悬停说明：选择器 -> 说明（同选择器多个元素都加）
  const NOTES = [
    ['#settings-btn', '点击弹出『配置』面板（主题色 / 语言切换）'],
    ['#run-toggle', '运行/停止：运行中点击=停止当前会话（POST /session/{sid}/cancel）'],
    ['.tb-right .ic-btn', '刷新状态/会话'],
    ['#preset-btn', '② 预设功能：对话开始后随时弹出预设卡片'],
    ['#model-chip', '模型档位下拉（GET /model-profiles）'],
    ['.new-conv', '新建会话 POST /session/new'],
    ['#add-model-btn', '点击弹出『添加模型』面板（模板）'],
    ['.send', '发送'],
    ['.fcard[data-preset="goal"]', '② 注入预设prompt：读 L3 goal SOP 自主达成目标'],
    ['.fcard[data-preset="explore"]', '② 注入预设prompt：自动浏览并周期汇总'],
    ['.fcard[data-preset="hive"]', '② 注入预设prompt：拉起多 worker 协同(Hive)'],
    ['.fcard[data-preset="review"]', '② 注入预设prompt：监察者模式严格验收'],
    ['.fcard[data-preset="mine"]', '② 用户自存的预设示例：自定义预设统一用星标图标'],
    ['.fcard[data-preset="add"]', '② 体现『一句话即一个功能』：用户自存任意预设 prompt 为新卡片'],
  ];

  // 内联「该放什么」卡片：宿主选择器 -> [位置, HTML]
  const INLINES = [
    ['.page[data-page="chat"] .msg-area', 'prepend', '该放：用户/助手消息气泡、turn 折叠、工具调用详情、流式增量；<b>消息内容须支持 Markdown + LaTeX + 代码高亮</b>。空会话展示下方预设功能'],
    ['.page[data-page="services"] .svc-panel[data-svc-panel="channels"]', 'append', '每行该放：渠道名、连接状态、启停开关、配置入口；对应 hub.pyw 的 imbot 进程。真实版用各渠道品牌 logo'],
    ['.page[data-page="services"] .svc-panel[data-svc-panel="status"]', 'append', '每行该放：进程名、PID、状态、CPU/内存、启停/重启、日志；至少含 bridge、各 imbot、定时调度'],
    ['.page[data-page="collab"] .page-pad', 'append', '该放：worker 卡片 + BBS 帖子流 + 选中 worker 的 output 流。数据源 temp/hive_*/、output.txt、agent_bbs；参考 conductor.py'],
    ['.page[data-page="collab"] .page-pad', 'append', '下方：BBS 帖子流 + 选中项 output.txt 实时滚动'],
    ['.page[data-page="token"] .page-pad', 'append', '该放：汇总卡 + 按会话明细表 + 趋势图。数据来自 bridge 的用量记录（_record_usage）'],
  ];

  const appEl = document.getElementById('app');
  if (!appEl) return;

  const fab = document.createElement('button');
  fab.id = 'toggle-annot';
  fab.className = 'annot-fab';
  document.body.appendChild(fab);

  let on = false;
  const injected = [];

  function annotate() {
    REGIONS.forEach(([sel, txt]) =>
      document.querySelectorAll(sel).forEach(el => el.setAttribute('data-region', txt)));
    NOTES.forEach(([sel, txt]) =>
      document.querySelectorAll(sel).forEach(el => el.setAttribute('data-note', txt)));
    // 上传按钮(composer-top 里非 #preset-btn 的 ic-btn) 与 Plan/Auto 特殊定位
    document.querySelectorAll('.composer-top .ic-btn').forEach(b => {
      if (b.id !== 'preset-btn') b.setAttribute('data-note', '附件/图片上传（多模态 [image:path]）');
    });
    const sm = document.querySelectorAll('.cb-left .chip.sm');
    if (sm[0]) sm[0].setAttribute('data-note', '② Plan：预设进入计划模式');
    if (sm[1]) sm[1].setAttribute('data-note', '② Auto：预设进入自主/goal 模式');
    INLINES.forEach(([sel, pos, html]) => {
      const host = document.querySelector(sel);
      if (!host) return;
      const d = document.createElement('div');
      d.className = 'annot-inline';
      d.innerHTML = html;
      if (pos === 'prepend') host.insertBefore(d, host.firstChild);
      else host.appendChild(d);
      injected.push(d);
    });
  }
  function clearAll() {
    document.querySelectorAll('[data-region]').forEach(el => el.removeAttribute('data-region'));
    document.querySelectorAll('[data-note]').forEach(el => el.removeAttribute('data-note'));
    injected.splice(0).forEach(d => d.remove());
  }
  function setLabel() { fab.textContent = '标注模式：' + (on ? '开' : '关'); }

  fab.addEventListener('click', () => {
    on = !on;
    if (on) { annotate(); appEl.classList.add('annot-on'); }
    else { appEl.classList.remove('annot-on'); clearAll(); }
    setLabel();
  });
  setLabel(); // 初始：关
})();
