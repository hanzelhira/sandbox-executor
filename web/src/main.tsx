import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './monaco-setup' // must precede any Monaco use: pins it to the local build
import './index.css'
import App from './App.tsx'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
