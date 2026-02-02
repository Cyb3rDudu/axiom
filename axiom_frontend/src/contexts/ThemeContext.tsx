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
  const { draftSettings, setDraftSettings, loadSettings } = useSettingsStore();

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

  // Load settings on mount only if not already loaded
  useEffect(() => {
    const loadSettingsIfNeeded = async () => {
      // Only load if we don't have settings yet
      if (!draftSettings) {
        await loadSettings();
      }
    };
    
    loadSettingsIfNeeded();
  }, []); // Empty dependency array - only run on mount

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
    
    setDraftSettings({
      ...draftSettings,
      appearance: {
        ...draftSettings.appearance,
        theme: newTheme,
      },
    });
  };

  const setColorScheme = (newScheme: ColorScheme) => {
    if (!draftSettings) return;
    
    setDraftSettings({
      ...draftSettings,
      appearance: {
        ...draftSettings.appearance,
        color_scheme: newScheme,
      },
    });
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
