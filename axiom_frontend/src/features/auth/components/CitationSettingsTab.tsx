import React, { useState, useEffect, useCallback } from 'react'
import { useSettingsStore } from './SettingsStore'
import { Card, CardContent, CardHeader, CardTitle } from '../../../components/ui/card'
import { Button } from '../../../components/ui/button'
import { Badge } from '../../../components/ui/badge'
import { Input } from '../../../components/ui/input'
import { Label } from '../../../components/ui/label'
import { Textarea } from '../../../components/ui/textarea'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '../../../components/ui/select'
import { Quote, Plus, Trash2, Star, Edit2, BookOpen } from 'lucide-react'
import { apiClient } from '../../../config/api'

interface CitationProfile {
  id: string
  name: string
  citation_mode: 'numbered' | 'author_year'
  in_text_rules: string
  bibliography_rules: string
  is_builtin: boolean
}

export const CitationSettingsTab: React.FC = () => {
  const { draftSettings, setDraftSettings } = useSettingsStore()
  const [profiles, setProfiles] = useState<CitationProfile[]>([])
  const [isLoadingProfiles, setIsLoadingProfiles] = useState(false)
  const [editingProfile, setEditingProfile] = useState<CitationProfile | null>(null)

  // New profile form state
  const [newProfile, setNewProfile] = useState({
    id: '',
    name: '',
    citation_mode: 'numbered' as 'numbered' | 'author_year',
    in_text_rules: '',
    bibliography_rules: '',
  })
  const [showCreateForm, setShowCreateForm] = useState(false)
  const [isSaving, setIsSaving] = useState(false)

  const defaultProfileId = draftSettings?.writing_settings?.default_citation_profile || null

  const fetchProfiles = useCallback(async () => {
    setIsLoadingProfiles(true)
    try {
      const response = await apiClient.get('/api/citation-profiles')
      setProfiles(response.data)
    } catch (error) {
      console.error('Failed to fetch citation profiles:', error)
    } finally {
      setIsLoadingProfiles(false)
    }
  }, [])

  useEffect(() => {
    fetchProfiles()
  }, [fetchProfiles])

  const handleSetDefault = (profileId: string) => {
    const newDefault = profileId === defaultProfileId ? null : profileId
    setDraftSettings({
      writing_settings: {
        ...(draftSettings?.writing_settings || {}),
        default_citation_profile: newDefault,
      },
    })
  }

  const handleCreateProfile = async () => {
    if (!newProfile.id || !newProfile.name) return
    setIsSaving(true)
    try {
      await apiClient.post('/api/citation-profiles', {
        id: newProfile.id,
        name: newProfile.name,
        citation_mode: newProfile.citation_mode,
        in_text_rules: newProfile.in_text_rules,
        bibliography_rules: newProfile.bibliography_rules,
      })
      setNewProfile({ id: '', name: '', citation_mode: 'numbered', in_text_rules: '', bibliography_rules: '' })
      setShowCreateForm(false)
      await fetchProfiles()
    } catch (error) {
      console.error('Failed to create citation profile:', error)
    } finally {
      setIsSaving(false)
    }
  }

  const handleDeleteProfile = async (profileId: string) => {
    try {
      await apiClient.delete(`/api/citation-profiles/${profileId}`)
      if (defaultProfileId === profileId) {
        setDraftSettings({
          writing_settings: {
            ...(draftSettings?.writing_settings || {}),
            default_citation_profile: null,
          },
        })
      }
      await fetchProfiles()
    } catch (error) {
      console.error('Failed to delete citation profile:', error)
    }
  }

  const handleUpdateProfile = async () => {
    if (!editingProfile) return
    setIsSaving(true)
    try {
      // POST handles upsert (replaces existing profile with same ID)
      await apiClient.post('/api/citation-profiles', {
        id: editingProfile.id,
        name: editingProfile.name,
        citation_mode: editingProfile.citation_mode,
        in_text_rules: editingProfile.in_text_rules,
        bibliography_rules: editingProfile.bibliography_rules,
      })
      setEditingProfile(null)
      await fetchProfiles()
    } catch (error) {
      console.error('Failed to update citation profile:', error)
    } finally {
      setIsSaving(false)
    }
  }

  if (!draftSettings) {
    return <div>Loading...</div>
  }

  return (
    <div className="space-y-4">
      {/* Header */}
      <Card>
        <CardHeader className="pb-3">
          <div className="flex items-center justify-between">
            <div>
              <CardTitle className="text-base flex items-center gap-2">
                <Quote className="h-4 w-4" />
                Citation Profiles
              </CardTitle>
              <p className="text-sm text-muted-foreground mt-1">
                Manage citation styles for research reports. Set a default profile or create custom ones.
              </p>
            </div>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setShowCreateForm(!showCreateForm)}
              className="flex items-center gap-2"
            >
              <Plus className="h-3 w-3" />
              New Profile
            </Button>
          </div>
        </CardHeader>

        <CardContent className="space-y-3">
          {/* Profile List */}
          {isLoadingProfiles ? (
            <p className="text-sm text-muted-foreground">Loading profiles...</p>
          ) : profiles.length === 0 ? (
            <p className="text-sm text-muted-foreground">No citation profiles found.</p>
          ) : (
            <div className="grid grid-cols-1 gap-3">
              {profiles.map((profile) => (
                <Card key={profile.id} className={`p-3 ${defaultProfileId === profile.id ? 'border-primary/50 bg-primary/5' : ''}`}>
                  <div className="flex items-start justify-between">
                    <div className="flex-1 min-w-0">
                      <div className="flex items-center gap-2 mb-1">
                        <h4 className="text-sm font-medium truncate">{profile.name}</h4>
                        <Badge variant={profile.is_builtin ? 'secondary' : 'outline'} className="text-xs shrink-0">
                          {profile.is_builtin ? 'Built-in' : 'Custom'}
                        </Badge>
                        {defaultProfileId === profile.id && (
                          <Badge variant="default" className="text-xs shrink-0">
                            Default
                          </Badge>
                        )}
                      </div>
                      <p className="text-xs text-muted-foreground mb-1">
                        Mode: {profile.citation_mode === 'numbered' ? 'Numbered' : 'Author-Year'}
                      </p>
                      {profile.in_text_rules && (
                        <p className="text-xs text-muted-foreground truncate">
                          {profile.in_text_rules.substring(0, 100)}{profile.in_text_rules.length > 100 ? '...' : ''}
                        </p>
                      )}
                    </div>
                    <div className="flex items-center gap-1 ml-2 shrink-0">
                      <Button
                        variant={defaultProfileId === profile.id ? 'default' : 'ghost'}
                        size="icon"
                        className="h-7 w-7"
                        onClick={() => handleSetDefault(profile.id)}
                        title={defaultProfileId === profile.id ? 'Remove as default' : 'Set as default'}
                      >
                        <Star className={`h-3.5 w-3.5 ${defaultProfileId === profile.id ? 'fill-current' : ''}`} />
                      </Button>
                      {!profile.is_builtin && (
                        <>
                          <Button
                            variant="ghost"
                            size="icon"
                            className="h-7 w-7"
                            onClick={() => setEditingProfile(profile)}
                            title="Edit profile"
                          >
                            <Edit2 className="h-3.5 w-3.5" />
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            className="h-7 w-7 text-destructive hover:text-destructive"
                            onClick={() => handleDeleteProfile(profile.id)}
                            title="Delete profile"
                          >
                            <Trash2 className="h-3.5 w-3.5" />
                          </Button>
                        </>
                      )}
                    </div>
                  </div>
                </Card>
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      {/* Create New Profile Form */}
      {showCreateForm && (
        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="text-base flex items-center gap-2">
              <BookOpen className="h-4 w-4" />
              Create Custom Profile
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1">
                <Label htmlFor="profile-id" className="text-xs">Profile ID (slug)</Label>
                <Input
                  id="profile-id"
                  value={newProfile.id}
                  onChange={(e) => setNewProfile({ ...newProfile, id: e.target.value.replace(/[^a-z0-9_]/g, '') })}
                  placeholder="e.g. my_harvard"
                  className="h-8 text-sm"
                />
              </div>
              <div className="space-y-1">
                <Label htmlFor="profile-name" className="text-xs">Name</Label>
                <Input
                  id="profile-name"
                  value={newProfile.name}
                  onChange={(e) => setNewProfile({ ...newProfile, name: e.target.value })}
                  placeholder="e.g. My Harvard Style"
                  className="h-8 text-sm"
                />
              </div>
            </div>
            <div className="space-y-1">
              <Label htmlFor="profile-mode" className="text-xs">Citation Mode</Label>
              <Select
                value={newProfile.citation_mode}
                onValueChange={(value: 'numbered' | 'author_year') => setNewProfile({ ...newProfile, citation_mode: value })}
              >
                <SelectTrigger className="h-8 text-sm">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="numbered">Numbered [1], [2], [3]</SelectItem>
                  <SelectItem value="author_year">Author-Year (Smith, 2024)</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1">
              <Label htmlFor="profile-intext" className="text-xs">In-Text Citation Rules</Label>
              <Textarea
                id="profile-intext"
                value={newProfile.in_text_rules}
                onChange={(e) => setNewProfile({ ...newProfile, in_text_rules: e.target.value })}
                placeholder="Describe how in-text citations should be formatted..."
                className="text-sm min-h-[80px]"
              />
            </div>
            <div className="space-y-1">
              <Label htmlFor="profile-bib" className="text-xs">Bibliography Rules</Label>
              <Textarea
                id="profile-bib"
                value={newProfile.bibliography_rules}
                onChange={(e) => setNewProfile({ ...newProfile, bibliography_rules: e.target.value })}
                placeholder="Describe how bibliography entries should be formatted..."
                className="text-sm min-h-[80px]"
              />
            </div>
            <div className="flex justify-end gap-2">
              <Button variant="outline" size="sm" onClick={() => setShowCreateForm(false)}>
                Cancel
              </Button>
              <Button
                size="sm"
                onClick={handleCreateProfile}
                disabled={!newProfile.id || !newProfile.name || isSaving}
              >
                {isSaving ? 'Saving...' : 'Create Profile'}
              </Button>
            </div>
          </CardContent>
        </Card>
      )}

      {/* Edit Profile Dialog (inline) */}
      {editingProfile && (
        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="text-base flex items-center gap-2">
              <Edit2 className="h-4 w-4" />
              Edit Profile: {editingProfile.name}
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="space-y-1">
              <Label htmlFor="edit-name" className="text-xs">Name</Label>
              <Input
                id="edit-name"
                value={editingProfile.name}
                onChange={(e) => setEditingProfile({ ...editingProfile, name: e.target.value })}
                className="h-8 text-sm"
              />
            </div>
            <div className="space-y-1">
              <Label htmlFor="edit-mode" className="text-xs">Citation Mode</Label>
              <Select
                value={editingProfile.citation_mode}
                onValueChange={(value: 'numbered' | 'author_year') => setEditingProfile({ ...editingProfile, citation_mode: value })}
              >
                <SelectTrigger className="h-8 text-sm">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="numbered">Numbered [1], [2], [3]</SelectItem>
                  <SelectItem value="author_year">Author-Year (Smith, 2024)</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1">
              <Label htmlFor="edit-intext" className="text-xs">In-Text Citation Rules</Label>
              <Textarea
                id="edit-intext"
                value={editingProfile.in_text_rules}
                onChange={(e) => setEditingProfile({ ...editingProfile, in_text_rules: e.target.value })}
                className="text-sm min-h-[80px]"
              />
            </div>
            <div className="space-y-1">
              <Label htmlFor="edit-bib" className="text-xs">Bibliography Rules</Label>
              <Textarea
                id="edit-bib"
                value={editingProfile.bibliography_rules}
                onChange={(e) => setEditingProfile({ ...editingProfile, bibliography_rules: e.target.value })}
                className="text-sm min-h-[80px]"
              />
            </div>
            <div className="flex justify-end gap-2">
              <Button variant="outline" size="sm" onClick={() => setEditingProfile(null)}>
                Cancel
              </Button>
              <Button size="sm" onClick={handleUpdateProfile} disabled={isSaving}>
                {isSaving ? 'Saving...' : 'Save Changes'}
              </Button>
            </div>
          </CardContent>
        </Card>
      )}
    </div>
  )
}
