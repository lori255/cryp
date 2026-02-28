import { Routes, Route, Navigate } from 'react-router-dom'
import Login from './pages/Login'
import FileBrowser from './pages/FileBrowser'

function App() {
  return (
    <Routes>
      <Route path="/" element={<Login />} />
      <Route path="/vault/:id" element={<FileBrowser />} />
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
}

export default App
