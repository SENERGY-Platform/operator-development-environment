import { cn } from "@/lib/utils"
import { Loader2Icon } from "lucide-react"

/**
 * Pins a spinner's rotation to the document's own clock.
 *
 * Two spinners that began turning at different moments run out of phase, and the
 * sessions panel stacks them: one row's "Working" leaning left while the row under
 * it leans right reads as two unrelated things happening, which is the one thing
 * they are not — they are the same answer to the same question, one per
 * conversation. Setting the start time to the timeline origin gives every spinner
 * on the page the same phase, whenever it mounted.
 *
 * Only at mount. A spinner whose animation is restarted later — the class going
 * away and coming back — drifts again, which no arrangement of one ref callback
 * can prevent and which no caller here does.
 *
 * Guarded rather than assumed: getAnimations is absent in jsdom, where the tests
 * render this, and it would throw on the first spinner of the first test.
 */
function syncSpin(node: Element | null): void {
  if (node === null || typeof node.getAnimations !== "function") return
  for (const animation of node.getAnimations()) animation.startTime = 0
}

function Spinner({ className, ref, ...props }: React.ComponentProps<"svg">) {
  return (
    <Loader2Icon
      data-slot="spinner"
      role="status"
      aria-label="Loading"
      className={cn("size-4 animate-spin", className)}
      // Composed rather than claimed: this component's own use of the ref is an
      // implementation detail, and a caller that wants the node still gets it.
      ref={(node: SVGSVGElement | null) => {
        syncSpin(node)
        if (typeof ref === "function") ref(node)
        else if (ref !== null && ref !== undefined) ref.current = node
      }}
      {...props}
    />
  )
}

export { Spinner, syncSpin }
