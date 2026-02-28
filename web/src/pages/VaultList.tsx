import { useEffect } from 'react'
import { useNavigate } from 'react-router-dom'

export default function VaultList() {
  const navigate = useNavigate()

  useEffect(() => {
    // Vault list is no longer used - redirect to login
    navigate('/', { replace: true })
  }, [navigate])

  return null
}
