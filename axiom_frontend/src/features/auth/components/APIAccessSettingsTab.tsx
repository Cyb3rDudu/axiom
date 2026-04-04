import React, { useState, useEffect, useCallback } from 'react'
import { Button } from '../../../components/ui/button'
import { Input } from '../../../components/ui/input'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '../../../components/ui/card'
import { Label } from '../../../components/ui/label'
import { Key, Copy, RefreshCw, Trash2, Check, AlertCircle, Terminal } from 'lucide-react'
import { apiClient } from '../../../config/api'

interface APIKeyState {
  maskedKey: string | null
  fullKey: string | null
  isLoading: boolean
  error: string | null
  copied: boolean
}

export const APIAccessSettingsTab: React.FC = () => {
  const [state, setState] = useState<APIKeyState>({
    maskedKey: null,
    fullKey: null,
    isLoading: false,
    error: null,
    copied: false,
  })

  const fetchKeyStatus = useCallback(async () => {
    try {
      setState(s => ({ ...s, isLoading: true, error: null }))
      const response = await apiClient.get('/api/v1/api-keys')
      setState(s => ({
        ...s,
        maskedKey: response.data.masked_key,
        isLoading: false,
      }))
    } catch (err: any) {
      setState(s => ({
        ...s,
        error: err.response?.data?.detail || 'Failed to load API key status',
        isLoading: false,
      }))
    }
  }, [])

  useEffect(() => {
    fetchKeyStatus()
  }, [fetchKeyStatus])

  const handleGenerate = async () => {
    try {
      setState(s => ({ ...s, isLoading: true, error: null, fullKey: null }))
      const response = await apiClient.post('/api/v1/api-keys')
      setState(s => ({
        ...s,
        fullKey: response.data.api_key,
        maskedKey: response.data.masked_key,
        isLoading: false,
      }))
    } catch (err: any) {
      setState(s => ({
        ...s,
        error: err.response?.data?.detail || 'Failed to generate API key',
        isLoading: false,
      }))
    }
  }

  const handleRevoke = async () => {
    try {
      setState(s => ({ ...s, isLoading: true, error: null }))
      await apiClient.delete('/api/v1/api-keys')
      setState(s => ({
        ...s,
        maskedKey: null,
        fullKey: null,
        isLoading: false,
      }))
    } catch (err: any) {
      setState(s => ({
        ...s,
        error: err.response?.data?.detail || 'Failed to revoke API key',
        isLoading: false,
      }))
    }
  }

  const handleCopy = async () => {
    const keyToCopy = state.fullKey || state.maskedKey
    if (!keyToCopy) return
    try {
      await navigator.clipboard.writeText(keyToCopy)
      setState(s => ({ ...s, copied: true }))
      setTimeout(() => setState(s => ({ ...s, copied: false })), 2000)
    } catch {
      // Fallback
    }
  }

  const baseUrl = window.location.origin

  const curlExample = `curl -X POST ${baseUrl}/api/v1/chat/completions \\
  -H "Authorization: Bearer YOUR_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "axiom",
    "messages": [{"role": "user", "content": "Summarize my documents"}]
  }'`

  const pythonExample = `from openai import OpenAI

client = OpenAI(
    base_url="${baseUrl}/api/v1",
    api_key="YOUR_API_KEY"
)

response = client.chat.completions.create(
    model="axiom",
    messages=[{"role": "user", "content": "What documents do I have?"}]
)
print(response.choices[0].message.content)`

  return (
    <div className="space-y-6">
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-foreground">
            <Key className="w-5 h-5" />
            API Key
          </CardTitle>
          <CardDescription>
            Generate an API key to access Axiom's document Q&A via the OpenAI-compatible API.
            This key allows programmatic access to your documents without the web UI.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          {state.error && (
            <div className="flex items-center gap-2 p-3 rounded-md bg-red-50 dark:bg-red-900/20 border border-red-200 dark:border-red-800">
              <AlertCircle className="w-4 h-4 text-red-500 flex-shrink-0" />
              <span className="text-sm text-red-700 dark:text-red-300">{state.error}</span>
            </div>
          )}

          {state.fullKey && (
            <div className="space-y-2">
              <Label className="text-foreground">Your new API key (shown only once):</Label>
              <div className="flex items-center gap-2">
                <Input
                  readOnly
                  value={state.fullKey}
                  className="font-mono text-sm bg-muted"
                />
                <Button variant="outline" size="icon" onClick={handleCopy} title="Copy to clipboard">
                  {state.copied ? <Check className="w-4 h-4 text-green-500" /> : <Copy className="w-4 h-4" />}
                </Button>
              </div>
              <p className="text-xs text-muted-foreground">
                Save this key securely. It will not be shown again.
              </p>
            </div>
          )}

          {!state.fullKey && state.maskedKey && (
            <div className="space-y-2">
              <Label className="text-foreground">Current API key:</Label>
              <div className="flex items-center gap-2">
                <Input
                  readOnly
                  value={state.maskedKey}
                  className="font-mono text-sm bg-muted"
                />
              </div>
            </div>
          )}

          <div className="flex items-center gap-2 pt-2">
            {!state.maskedKey ? (
              <Button onClick={handleGenerate} disabled={state.isLoading}>
                <Key className="w-4 h-4 mr-2" />
                Generate API Key
              </Button>
            ) : (
              <>
                <Button onClick={handleGenerate} variant="outline" disabled={state.isLoading}>
                  <RefreshCw className="w-4 h-4 mr-2" />
                  Regenerate
                </Button>
                <Button onClick={handleRevoke} variant="destructive" disabled={state.isLoading}>
                  <Trash2 className="w-4 h-4 mr-2" />
                  Revoke
                </Button>
              </>
            )}
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-foreground">
            <Terminal className="w-5 h-5" />
            Usage Examples
          </CardTitle>
          <CardDescription>
            The API is compatible with the OpenAI SDK. Replace YOUR_API_KEY with your actual key.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="space-y-2">
            <Label className="text-foreground font-medium">curl</Label>
            <pre className="p-3 rounded-md bg-muted text-sm font-mono overflow-x-auto whitespace-pre-wrap text-foreground">
              {curlExample}
            </pre>
          </div>

          <div className="space-y-2">
            <Label className="text-foreground font-medium">Python (openai SDK)</Label>
            <pre className="p-3 rounded-md bg-muted text-sm font-mono overflow-x-auto whitespace-pre-wrap text-foreground">
              {pythonExample}
            </pre>
          </div>

          <div className="space-y-2">
            <Label className="text-foreground font-medium">Extra parameters</Label>
            <p className="text-sm text-muted-foreground">
              You can pass <code className="px-1 py-0.5 rounded bg-muted text-foreground">document_group_id</code> in
              the request body to restrict the search to a specific document group. The response
              includes an extra <code className="px-1 py-0.5 rounded bg-muted text-foreground">sources</code> array
              with document provenance.
            </p>
          </div>
        </CardContent>
      </Card>
    </div>
  )
}
