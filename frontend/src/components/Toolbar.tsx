import * as Ico from "../icons";

interface Props {
  canResume: boolean;
  canStop: boolean;
  hasSelection: boolean;
  hasCompleted: boolean;
  onAdd: () => void;
  onResume: () => void;
  onStop: () => void;
  onStartAll: () => void;
  onStopAll: () => void;
  onDelete: () => void;
  onDeleteCompleted: () => void;
  onOpenFolder: () => void;
  onOptions: () => void;
}

function Btn({
  icon,
  label,
  onClick,
  disabled,
}: {
  icon: React.ReactNode;
  label: string;
  onClick: () => void;
  disabled?: boolean;
}) {
  return (
    <button className="tb-btn" onClick={onClick} disabled={disabled} title={label}>
      {icon}
      <span>{label}</span>
    </button>
  );
}

export default function Toolbar(p: Props) {
  return (
    <div className="toolbar">
      <Btn icon={<Ico.AddUrl />} label="添加 URL" onClick={p.onAdd} />
      <div className="tb-sep" />
      <Btn icon={<Ico.Resume />} label="开始" onClick={p.onResume} disabled={!p.canResume} />
      <Btn icon={<Ico.Stop />} label="暂停" onClick={p.onStop} disabled={!p.canStop} />
      <Btn icon={<Ico.StartAll />} label="全部开始" onClick={p.onStartAll} />
      <Btn icon={<Ico.StopAll />} label="全部暂停" onClick={p.onStopAll} />
      <div className="tb-sep" />
      <Btn icon={<Ico.Delete />} label="删除" onClick={p.onDelete} disabled={!p.hasSelection} />
      <Btn icon={<Ico.DeleteDone />} label="删除已完成" onClick={p.onDeleteCompleted} disabled={!p.hasCompleted} />
      <div className="tb-sep" />
      <Btn icon={<Ico.FolderOpen />} label="打开目录" onClick={p.onOpenFolder} disabled={!p.hasSelection} />
      <div className="tb-spacer" />
      <Btn icon={<Ico.Options />} label="选项" onClick={p.onOptions} />
    </div>
  );
}
