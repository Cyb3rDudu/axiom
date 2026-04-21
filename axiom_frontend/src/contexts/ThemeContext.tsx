import React, { createContext, useContext, useEffect, useCallback } from 'react';
import { useSettingsStore } from '../features/auth/components/SettingsStore';

type Theme = 'light' | 'dark';
export type ColorScheme = 'default' | 'blue' | 'emerald' | 'purple' | 'rose' | 'amber' | 'teal';

interface ThemeContextType {
  theme: Theme;
  colorScheme: ColorScheme;
  setTheme: (theme: Theme) => void;
  setColorScheme: (scheme: ColorScheme) => void;
  getThemeClasses: () => string;
}

const ThemeContext = createContext<ThemeContextType | undefined>(undefined);

export const ThemeProvider: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  const { draftSettings, setDraftSettings, loadSettings, persistAppearance } = useSettingsStore();

  // Get theme from settings or localStorage fallback
  const theme = draftSettings?.appearance?.theme ||
    (typeof window !== 'undefined' ? (localStorage.getItem('theme') as Theme) : null) ||
    'light';
  const colorScheme = draftSettings?.appearance?.color_scheme ||
    (typeof window !== 'undefined' ? (localStorage.getItem('colorScheme') as ColorScheme) : null) ||
    'default';

  const getThemeClasses = useCallback(() => {
    let classes = theme;
    
    if (colorScheme !== 'default') {
      if (theme === 'light') {
        classes += ` theme-light-${colorScheme}`;
      } else {
        classes += ` theme-dark-${colorScheme}`;
      }
    }
    
    return classes;
  }, [theme, colorScheme]);

  // Load settings on mount - always ensure fresh settings
  useEffect(() => {
    // Always load settings when ThemeProvider mounts to ensure theme is applied
    loadSettings();
  }, [loadSettings]);

  // Apply theme classes to document root and persist to localStorage
  useEffect(() => {
    const root = document.documentElement;
    const themeClasses = getThemeClasses();

    // Remove all existing theme classes
    root.classList.remove('light', 'dark');
    root.classList.remove(
      'theme-light-blue', 'theme-light-emerald', 'theme-light-purple', 'theme-light-rose',
      'theme-light-amber', 'theme-light-teal',
      'theme-dark-blue', 'theme-dark-emerald', 'theme-dark-purple', 'theme-dark-rose',
      'theme-dark-amber', 'theme-dark-teal'
    );

    // Add current theme classes
    themeClasses.split(' ').forEach(cls => {
      if (cls.trim()) {
        root.classList.add(cls.trim());
      }
    });

    // Persist to localStorage for quick access on next load
    if (typeof window !== 'undefined') {
      localStorage.setItem('theme', theme);
      localStorage.setItem('colorScheme', colorScheme);
    }
  }, [theme, colorScheme, getThemeClasses]);

  const setTheme = (newTheme: Theme) => {
    if (!draftSettings) return;

    const nextAppearance = {
      ...draftSettings.appearance,
      theme: newTheme,
    };
    setDraftSettings({
      ...draftSettings,
      appearance: nextAppearance,
    });
    // Fire-and-forget persistence. The store does an optimistic update and
    // reverts on failure, so we don't need to await or catch here — without
    // this call the switch would snap back after logout (only draftSettings
    // gets mutated, never the backend).
    void persistAppearance(nextAppearance);
  };

  const setColorScheme = (newScheme: ColorScheme) => {
    if (!draftSettings) return;

    const nextAppearance = {
      ...draftSettings.appearance,
      color_scheme: newScheme,
    };
    setDraftSettings({
      ...draftSettings,
      appearance: nextAppearance,
    });
    void persistAppearance(nextAppearance);
  };

  return (
    <ThemeContext.Provider value={{ theme, colorScheme, setTheme, setColorScheme, getThemeClasses }}>
      {children}
    </ThemeContext.Provider>
  );
};

export const useTheme = () => {
  const context = useContext(ThemeContext);
  if (context === undefined) {
    throw new Error('useTheme must be used within a ThemeProvider');
  }
  return context;
};
