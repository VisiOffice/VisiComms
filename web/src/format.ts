// Short, plain durations for a queue: "3h 12m", "48m", "2d 4h".
export function duration(ms: number): string {
  const minutes = Math.max(0, Math.round(ms / 60000));
  if (minutes < 60) {
    return `${minutes}m`;
  }
  const hours = Math.floor(minutes / 60);
  if (hours < 24) {
    const m = minutes % 60;
    return m ? `${hours}h ${m}m` : `${hours}h`;
  }
  const days = Math.floor(hours / 24);
  const h = hours % 24;
  return h ? `${days}d ${h}h` : `${days}d`;
}

export function waited(since: string, now = Date.now()): string {
  return duration(now - new Date(since).getTime());
}

// "Due in 48m", or "Overdue 12m" once the deadline has passed.
export function dueLabel(due: string | undefined, now = Date.now()): {text: string; isLate: boolean; isSoon: boolean} | null {
  if (!due) {
    return null;
  }
  const left = new Date(due).getTime() - now;
  if (left < 0) {
    return {text: `Overdue ${duration(-left)}`, isLate: true, isSoon: false};
  }
  return {text: `Due in ${duration(left)}`, isLate: false, isSoon: left < 60 * 60000};
}
