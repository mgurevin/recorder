import { Moon, Rows3, Sun } from "lucide-react";
import { useEffect, useState } from "react";

type Theme = "system" | "dark" | "light";
type Density = "compact" | "comfortable";

const THEME_KEY = "recorder.inspector.theme";
const DENSITY_KEY = "recorder.inspector.density";

function storedValue<T extends string>(key: string, allowed: readonly T[], fallback: T): T {
  const value = window.localStorage.getItem(key);

  return allowed.includes(value as T) ? value as T : fallback;
}

export function AppearanceControls() {
  const [theme, setTheme] = useState<Theme>(() => storedValue(THEME_KEY, ["system", "dark", "light"], "system"));
  const [density, setDensity] = useState<Density>(() => storedValue(DENSITY_KEY, ["compact", "comfortable"], "compact"));

  useEffect(() => {
    const root = document.documentElement;
    if (theme === "system") root.removeAttribute("data-theme");
    else root.dataset.theme = theme;
    window.localStorage.setItem(THEME_KEY, theme);
  }, [theme]);

  useEffect(() => {
    document.documentElement.dataset.density = density;
    window.localStorage.setItem(DENSITY_KEY, density);
  }, [density]);

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
      <button
        type="button"
        className={`icon-btn ${density === "comfortable" ? "active" : ""}`}
        data-tooltip={`Density: ${density}`}
        aria-label={`Use ${density === "compact" ? "comfortable" : "compact"} density`}
        onClick={() => setDensity(density === "compact" ? "comfortable" : "compact")}
      >
        <Rows3 size={15} />
      </button>
    </div>
  );
}
