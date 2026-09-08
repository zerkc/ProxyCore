import { useState } from "react";

export type PasswordInputProps = {
  id?: string;
  value: string;
  onChange: (value: string) => void;
  label?: string;
  autoComplete?: string;
  required?: boolean;
  minLength?: number;
  className?: string;
  disabled?: boolean;
};

const EyeIcon = () => (
  <svg
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth="2"
    strokeLinecap="round"
    strokeLinejoin="round"
    width="18"
    height="18"
    aria-hidden="true"
  >
    <path d="M2 12s3-7 10-7 10 7 10 7-3 7-10 7-10-7-10-7Z" />
    <circle cx="12" cy="12" r="3" />
  </svg>
);

const EyeOffIcon = () => (
  <svg
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth="2"
    strokeLinecap="round"
    strokeLinejoin="round"
    width="18"
    height="18"
    aria-hidden="true"
  >
    <path d="M9.88 5.05A10.94 10.94 0 0 1 12 5c7 0 10 7 10 7a13.16 13.16 0 0 1-2.16 2.94" />
    <path d="M6.61 6.61A13.5 13.5 0 0 0 2 12s3 7 10 7a10.7 10.7 0 0 0 5.39-1.39" />
    <path d="m1 1 22 22" />
    <path d="M14.12 14.12A3 3 0 1 1 9.88 9.88" />
  </svg>
);

export function PasswordInput(props: PasswordInputProps) {
  const [visible, setVisible] = useState(false);

  const input = (
    <div className="relative">
      <input
        id={props.id}
        type={visible ? "text" : "password"}
        value={props.value}
        onChange={(event) => props.onChange(event.target.value)}
        autoComplete={props.autoComplete ?? "current-password"}
        required={props.required}
        minLength={props.minLength}
        disabled={props.disabled}
        className={`${props.className ?? ""} pr-10`}
      />
      <button
        type="button"
        aria-label={visible ? "Hide password" : "Show password"}
        aria-pressed={visible}
        tabIndex={0}
        onClick={() => setVisible((v) => !v)}
        className="absolute inset-y-0 right-1 my-1 inline-flex items-center justify-center rounded-none px-2 text-mute transition hover:text-mist focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-signal"
      >
        {visible ? <EyeOffIcon /> : <EyeIcon />}
      </button>
    </div>
  );

  if (props.label) {
    return (
      <label className="pc-label">
        {props.label}
        {input}
      </label>
    );
  }

  return input;
}
