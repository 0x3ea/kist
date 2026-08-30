import {createApp} from 'vue'
import App from './App.vue'
import './style.css';

// 全局抑制 webkit2gtk 原生右键菜单(后退/前进/停止/重新加载/检查元素是网页
// 行为,与桌面应用形态相悖;信号层定制拿不到 WebKitWebView 句柄,JS 层
// preventDefault 是唯一现实路径,见 docs/todo/20)。文件页的自绘菜单在
// 组件层另行挂接,不受影响。代价:输入框内原生剪切/复制/粘贴项一并消失
// (Ctrl+V 不受影响),立项时已接受。
document.addEventListener('contextmenu', (e) => e.preventDefault())

createApp(App).mount('#app')
