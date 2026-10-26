;; SPDX-License-Identifier: MPL-2.0

;; Run by the pinned Guix's REPL, never by the runner's host Guile.
(use-modules (guix ui)
             (guix discovery)
             (guix platform)
             (gnu image)
             (gnu system image)
             (ice-9 match)
             (srfi srfi-1))

(define (include-definitions! known custom)
  (unless (null? custom)
    ;; Compiled Guix procedures may retain the original cached collection even
    ;; if its module binding is replaced. Extend that collection in place so
    ;; both compiled callers and interpreted callers see the loaded definitions.
    ;; Guix supplies nonempty built-in image and platform collections.
    (when (null? known)
      (error "cannot extend an empty Guix definition collection"))
    (set-cdr! known (append (cdr custom) (cons (car known) (cdr known))))
    (set-car! known (car custom))))

(define (register-definitions file)
  (let* ((interface
          (save-module-excursion
           (lambda ()
             (set-current-module (make-fresh-user-module))
             ;; Unlike load, primitive-load leaves the declared module current,
             ;; so its public interface can be collected without guessing its name.
             (primitive-load file)
             (module-public-interface (current-module)))))
         (collect (lambda (predicate)
                    (if interface
                        (fold-module-public-variables
                         (lambda (object result)
                           (if (predicate object) (cons object result) result))
                         '() (list interface))
                        '())))
         (custom-platforms (collect platform?))
         (custom-images (collect image-type?)))
    ;; Discovery normally only visits modules installed in a Guix profile.
    ;; Extend the lookup collections explicitly, including already-cached ones.
    (unless (null? custom-platforms)
      (include-definitions! (platforms) custom-platforms))
    (unless (null? custom-images)
      (include-definitions! (force %image-types) custom-images))))

(with-error-handling
  (match (cdr (command-line))
    ((system-definition image-definition command args ...)
     ;; Platforms must be registered before loading dependent image definitions,
     ;; and before the command parses --system.
     (for-each register-definitions
               (delete-duplicates
                (remove string-null? (list system-definition image-definition))))
     (apply run-guix-command (string->symbol command) args))))
