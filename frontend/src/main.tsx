import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'
import AddPage from './AddPage'

// The dedicated "add download" window is opened at /?view=add; everything else
// is the main application window.
const view = new URLSearchParams(window.location.search).get('view')
const Root = view === 'add' ? AddPage : App

ReactDOM.createRoot(document.getElementById('root') as HTMLElement).render(
  <React.StrictMode>
    <Root />
  </React.StrictMode>,
)
