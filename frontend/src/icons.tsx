// Inline SVG icons styled to resemble IDM's colorful toolbar/category icons.
// Kept dependency-free and small.

interface P {
  size?: number;
}

const wrap = (size: number, children: React.ReactNode) => (
  <svg width={size} height={size} viewBox="0 0 24 24" fill="none" xmlns="http://www.w3.org/2000/svg">
    {children}
  </svg>
);

/* ---------- Toolbar icons ---------- */

export const AddUrl = ({ size = 22 }: P) =>
  wrap(size, (
    <>
      <circle cx="11" cy="10" r="7" stroke="#2a7de1" strokeWidth="1.6" />
      <path d="M4 10h14M11 3c2.2 2 2.2 12 0 14M11 3c-2.2 2-2.2 12 0 14" stroke="#2a7de1" strokeWidth="1.2" />
      <circle cx="18" cy="17" r="5.5" fill="#2db84d" />
      <path d="M18 14.5v5M15.5 17h5" stroke="#fff" strokeWidth="1.8" strokeLinecap="round" />
    </>
  ));

export const Resume = ({ size = 22 }: P) =>
  wrap(size, (
    <>
      <circle cx="12" cy="12" r="10" fill="#2db84d" />
      <path d="M10 8l6 4-6 4V8z" fill="#fff" />
    </>
  ));

export const Stop = ({ size = 22 }: P) =>
  wrap(size, (
    <>
      <circle cx="12" cy="12" r="10" fill="#e8a33d" />
      <rect x="8.5" y="8.5" width="3" height="7" rx="1" fill="#fff" />
      <rect x="12.5" y="8.5" width="3" height="7" rx="1" fill="#fff" />
    </>
  ));

export const StartAll = ({ size = 22 }: P) =>
  wrap(size, (
    <>
      <path d="M4 7l6 5-6 5V7z" fill="#2db84d" />
      <path d="M12 7l6 5-6 5V7z" fill="#2db84d" />
    </>
  ));

export const StopAll = ({ size = 22 }: P) =>
  wrap(size, (
    <>
      <rect x="4" y="6" width="6" height="12" rx="1.5" fill="#e8a33d" />
      <rect x="13" y="6" width="6" height="12" rx="1.5" fill="#e8a33d" />
    </>
  ));

export const Delete = ({ size = 22 }: P) =>
  wrap(size, (
    <>
      <path d="M6 7h12l-1 13H7L6 7z" fill="#e25b5b" />
      <path d="M9 7V5h6v2" stroke="#b53a3a" strokeWidth="1.6" />
      <path d="M4 7h16" stroke="#b53a3a" strokeWidth="1.6" strokeLinecap="round" />
      <path d="M10 10v7M14 10v7" stroke="#fff" strokeWidth="1.3" strokeLinecap="round" />
    </>
  ));

export const DeleteDone = ({ size = 22 }: P) =>
  wrap(size, (
    <>
      <path d="M6 7h12l-1 13H7L6 7z" fill="#9aa3ad" />
      <path d="M4 7h16" stroke="#6b7480" strokeWidth="1.6" strokeLinecap="round" />
      <circle cx="17" cy="17" r="5" fill="#2db84d" />
      <path d="M14.7 17l1.6 1.6 3-3.2" stroke="#fff" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
    </>
  ));

export const Options = ({ size = 22 }: P) =>
  wrap(size, (
    <>
      <path
        d="M12 8.5a3.5 3.5 0 100 7 3.5 3.5 0 000-7z"
        stroke="#5a6573"
        strokeWidth="1.6"
      />
      <path
        d="M12 2.5l1.3 2.2 2.5-.5.3 2.5 2.3 1-.8 2.4 1.7 1.9-1.7 1.9.8 2.4-2.3 1-.3 2.5-2.5-.5L12 21.5l-1.3-2.2-2.5.5-.3-2.5-2.3-1 .8-2.4L4.4 12l1.7-1.9-.8-2.4 2.3-1 .3-2.5 2.5.5L12 2.5z"
        stroke="#5a6573"
        strokeWidth="1.3"
        strokeLinejoin="round"
        fill="#eef1f5"
      />
      <circle cx="12" cy="12" r="3" fill="#5a6573" />
    </>
  ));

export const FolderOpen = ({ size = 22 }: P) =>
  wrap(size, (
    <>
      <path d="M3 6h6l2 2h10v3H3V6z" fill="#e8b54a" />
      <path d="M3 9h18l-2 9H5L3 9z" fill="#f5cd6b" />
    </>
  ));

/* ---------- Category icons (16px) ---------- */

export const CatAll = ({ size = 16 }: P) =>
  wrap(size, (
    <>
      <path d="M12 3v9m0 0l-4-4m4 4l4-4" stroke="#2a7de1" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M4 16h16v3a1 1 0 01-1 1H5a1 1 0 01-1-1v-3z" fill="#2a7de1" />
    </>
  ));

export const CatUnfinished = ({ size = 16 }: P) =>
  wrap(size, (
    <>
      <circle cx="12" cy="12" r="9" stroke="#e8a33d" strokeWidth="2" />
      <path d="M12 7v5l3 2" stroke="#e8a33d" strokeWidth="2" strokeLinecap="round" />
    </>
  ));

export const CatFinished = ({ size = 16 }: P) =>
  wrap(size, (
    <>
      <circle cx="12" cy="12" r="9" fill="#2db84d" />
      <path d="M8 12l2.5 2.5L16 9" stroke="#fff" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
    </>
  ));

export const CatCompressed = ({ size = 16 }: P) =>
  wrap(size, (
    <>
      <rect x="4" y="4" width="16" height="16" rx="2" fill="#b58b4a" />
      <path d="M11 4v3m2 1v3m-2 1v3" stroke="#fff" strokeWidth="1.6" />
      <rect x="10" y="14" width="4" height="4" rx="1" fill="#fff" />
    </>
  ));

export const CatDocuments = ({ size = 16 }: P) =>
  wrap(size, (
    <>
      <path d="M6 3h8l4 4v14H6V3z" fill="#4a90d9" />
      <path d="M14 3v4h4" fill="#2a6cb0" />
      <path d="M8 12h8M8 15h8M8 18h5" stroke="#fff" strokeWidth="1.3" strokeLinecap="round" />
    </>
  ));

export const CatMusic = ({ size = 16 }: P) =>
  wrap(size, (
    <>
      <path d="M9 17V6l9-2v9" stroke="#8a5cd6" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
      <circle cx="7" cy="17" r="2.5" fill="#8a5cd6" />
      <circle cx="16" cy="15" r="2.5" fill="#8a5cd6" />
    </>
  ));

export const CatVideo = ({ size = 16 }: P) =>
  wrap(size, (
    <>
      <rect x="3" y="6" width="18" height="12" rx="2" fill="#d9534f" />
      <path d="M11 9l4 3-4 3V9z" fill="#fff" />
    </>
  ));

export const CatPrograms = ({ size = 16 }: P) =>
  wrap(size, (
    <>
      <rect x="3" y="4" width="18" height="16" rx="2" fill="#5a6573" />
      <path d="M3 8h18" stroke="#fff" strokeWidth="1.4" />
      <circle cx="6" cy="6" r="0.9" fill="#fff" />
      <circle cx="9" cy="6" r="0.9" fill="#fff" />
      <rect x="7" y="11" width="10" height="2" rx="1" fill="#9aa3ad" />
    </>
  ));

export const categoryIcon: Record<string, (p: P) => any> = {
  General: CatDocuments,
  Compressed: CatCompressed,
  Documents: CatDocuments,
  Music: CatMusic,
  Video: CatVideo,
  Programs: CatPrograms,
};
