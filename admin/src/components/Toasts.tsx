import { Icon } from "./Icons";

export function Toasts(props: { notice?: string; error?: string }) {
  if (!props.notice && !props.error) return null;

  return (
    <div class="toast-region" aria-live="polite" aria-atomic="true">
      {props.notice && (
        <div class="toast toast-success" role="status" data-toast data-toast-timeout="5000">
          <Icon name="circle-check" />
          <span>{props.notice}</span>
          <button type="button" class="toast-close" data-toast-close aria-label="Dismiss notification">
            <Icon name="x" />
          </button>
        </div>
      )}
      {props.error && (
        <div class="toast toast-error" role="alert" data-toast>
          <Icon name="alert-triangle" />
          <span>{props.error}</span>
          <button type="button" class="toast-close" data-toast-close aria-label="Dismiss notification">
            <Icon name="x" />
          </button>
        </div>
      )}
    </div>
  );
}
