export function FolderIcon() {
  return (
    <svg class="ic ic-dir" viewBox="0 0 16 16" aria-hidden="true">
      <path
        fill="currentColor"
        d="M1.5 4.25c0-.69.56-1.25 1.25-1.25h2.84c.4 0 .77.18 1.02.49l.63.76h4.51c.69 0 1.25.56 1.25 1.25v6c0 .69-.56 1.25-1.25 1.25H2.75c-.69 0-1.25-.56-1.25-1.25z"
      />
    </svg>
  );
}

export function FileIcon() {
  return (
    <svg class="ic ic-file" viewBox="0 0 16 16" aria-hidden="true">
      <path fill="none" stroke="currentColor" stroke-width="1.1" stroke-linejoin="round" d="M4 2.5h5l3 3v8h-8z" />
      <path fill="none" stroke="currentColor" stroke-width="1.1" stroke-linejoin="round" d="M9 2.5v3h3" />
    </svg>
  );
}

/**
 * Tabler icon.
 *
 * Always aria-hidden: an icon font renders private-use codepoints, which a
 * screen reader would otherwise read as garbage. Meaning comes from the adjacent
 * label, or from aria-label on the button when the label is visual only.
 */
export function Icon(props: { name: string; class?: string }) {
  return <i class={`ti ti-${props.name}${props.class ? ` ${props.class}` : ""}`} aria-hidden="true" />;
}

/**
 * Button content: icon plus label, where the label collapses on narrow screens.
 *
 * The label stays in the DOM rather than being dropped, so the accessible name
 * is unchanged regardless of viewport.
 */
export function IconLabel(props: { icon: string; children: string }) {
  return (
    <>
      <Icon name={props.icon} />
      <span class="btn-label">{props.children}</span>
    </>
  );
}
