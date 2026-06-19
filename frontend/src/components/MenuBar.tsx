import { useEffect, useRef, useState } from "react";

export interface MenuAction {
  label: string;
  shortcut?: string;
  onClick?: () => void;
  disabled?: boolean;
  separator?: boolean;
}

interface Menu {
  title: string;
  items: MenuAction[];
}

interface Props {
  menus: Menu[];
}

export default function MenuBar({ menus }: Props) {
  const [open, setOpen] = useState<number | null>(null);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (open === null) return;
    const close = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(null);
    };
    window.addEventListener("mousedown", close);
    return () => window.removeEventListener("mousedown", close);
  }, [open]);

  return (
    <div className="menubar" ref={ref}>
      {menus.map((m, i) => (
        <div key={m.title} style={{ position: "relative" }}>
          <div
            className={`menu-item${open === i ? " open" : ""}`}
            onClick={() => setOpen(open === i ? null : i)}
            onMouseEnter={() => open !== null && setOpen(i)}
          >
            {m.title}
          </div>
          {open === i && (
            <div className="menu-dropdown">
              {m.items.map((it, j) =>
                it.separator ? (
                  <div key={j} className="sep" />
                ) : (
                  <div
                    key={j}
                    className={`mi${it.disabled ? " disabled" : ""}`}
                    onClick={() => {
                      if (it.disabled) return;
                      setOpen(null);
                      it.onClick?.();
                    }}
                  >
                    <span>{it.label}</span>
                    {it.shortcut && <span className="sc">{it.shortcut}</span>}
                  </div>
                )
              )}
            </div>
          )}
        </div>
      ))}
    </div>
  );
}
