import { Moon, Sun } from "lucide-react";
import { useEffect, useState } from "react";

type Theme = "system" | "dark" | "light";

const THEME_KEY = "recorder.inspector.theme";

function storedValue<T extends string>(key: string, allowed: readonly T[], fallback: T): T {
  const value = window.localStorage.getItem(key);

  return allowed.includes(value as T) ? value as T : fallback;
}

export function AppearanceControls() {
  const [theme, setTheme] = useState<Theme>(() => storedValue(THEME_KEY, ["system", "dark", "light"], "system"));

  useEffect(() => {
    const root = document.documentElement;
    if (theme === "system") root.removeAttribute("data-theme");
    else root.dataset.theme = theme;
    window.localStorage.setItem(THEME_KEY, theme);
  }, [theme]);

  const nextTheme: Record<Theme, Theme> = { system: "dark", dark: "light", light: "system" };
  const themeLabel = theme === "system" ? "system theme" : `${theme} theme`;

  return (
    <div className="appearance-controls" aria-label="Appearance settings">
      <button
        type="button"
        className="icon-btn"
        data-tooltip={`Theme: ${theme}. Click for ${nextTheme[theme]}.`}
        aria-label={`${themeLabel}; switch to ${nextTheme[theme]}`}
        onClick={() => setTheme(nextTheme[theme])}
      >
        {theme === "light" ? <Sun size={15} /> : <Moon size={15} />}
      </button>
    </div>
  );
}
