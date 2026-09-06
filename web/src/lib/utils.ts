import { clsx, type ClassValue } from "clsx"
import { twMerge } from "tailwind-merge"

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

// sleep pauses for ms — the interval primitive shared by the frontend's
// poll loops (api.ts insights/pending polling, diary-view refresh).
export const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))
