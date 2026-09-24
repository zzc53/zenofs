import { render } from 'preact'
import './styles.css'
import { App } from './app'
import { startRouter } from './router'
import { bootstrap } from './store'

startRouter()
render(<App />, document.getElementById('app') as HTMLElement)

// 启动即问一次后端状态：决定是走首启向导还是登录页
void bootstrap()
