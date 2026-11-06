;;; SPDX-License-Identifier: MPL-2.0
(define-module (guix-secrets helper)
  #:use-module (gnu packages linux)
  #:use-module (guix gexp)
  #:use-module (guix modules)
  #:use-module (guix monads)
  #:use-module (guix store)
  #:use-module (guix derivations)
  #:export (image-helper))

(define (image-helper)
  (let ((program
         (program-file
            "guix-secrets-image-helper"
          (with-imported-modules
              (source-module-closure '((guix-secrets privileged))
                                     #:select? (lambda (name) (eq? 'guix-secrets (car name))))
            #~(begin
                (use-modules (guix-secrets privileged) (ice-9 match))
                (sigaction SIGTERM (lambda _ (throw 'provisioning-cancelled)))
                (sigaction SIGINT (lambda _ (throw 'provisioning-cancelled)))
                (catch #t
                  (lambda ()
                    (set-port-encoding! (current-input-port) "UTF-8")
                    (match (read)
                      ((image offset size files)
                       (provision-with-mount image offset size files
                                             #$(file-append util-linux "/bin/mount")
                                             #$(file-append util-linux "/bin/umount")))
                      (_ (error "invalid provisioning request"))))
                  (lambda (key . args)
                    (display "guix secrets: privileged provisioning failed\n"
                             (current-error-port))
                    (exit (if (eq? key 'mount-cleanup-failed) 2 1)))))))))
    (with-store store
      (run-with-store store
        (mlet %store-monad ((drv (lower-object program)))
          (mbegin %store-monad
            (built-derivations (list drv))
            (return (derivation->output-path drv))))))))
