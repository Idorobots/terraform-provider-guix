;;; SPDX-License-Identifier: MPL-2.0
;;; Integration driver. The fake backends test selection/publication/failure;
;;; action "real" uses an actual FUSE mount and never opens a Polkit prompt.
(use-modules (guix-secrets image) (guix-secrets manifest)
             (ice-9 match) (rnrs io ports) (rnrs bytevectors))

(match (cdr (command-line))
  ((action input output manifest)
   (define image-module (resolve-module '(guix-secrets image)))
   (define (mark-image image)
     (let ((port (open-file image "r+b")))
       (seek port (+ (* 2 1024 1024) (* 31 1024 1024)) SEEK_SET)
       (put-bytevector port #vu8(1 2 3 4))
       (close-port port)))
   (cond
    ((string=? action "inspect")
     (write (image-partition input)) (newline))
    ((string=? action "select")
     (write (image-partition input "Guix")) (newline))
    (else
     (cond
      ((string=? action "fuse")
       (module-set! image-module 'call-with-fuse-mount
                    (lambda (image offset mountpoint log proc)
                      (unless (= offset (* 2 1024 1024)) (error "wrong offset"))
                      (mark-image image) #t))
       (module-set! image-module 'polkit-provision (lambda _ (error "unexpected escalation"))))
      ((member action '("polkit" "cancel"))
       (module-set! image-module 'call-with-fuse-mount (lambda _ #f))
       (module-set! image-module 'polkit-provision
                    (lambda (image partition files)
                      (unless (and (equal? partition '(2097152 33554432))
                                   (string=? "/etc/private/key" (caar files)))
                        (error "incorrect privileged request"))
                      (if (string=? action "cancel")
                          (error "authentication cancelled")
                          (mark-image image)))))
      ((string=? action "corrupt")
       (module-set! image-module 'call-with-fuse-mount (lambda _ (error "bad filesystem")))
       (module-set! image-module 'polkit-provision (lambda _ (exit 99))))
      ((string=? action "pkexec")
       (module-set! image-module 'call-with-fuse-mount (lambda _ #f)))
      ((string=? action "real")
       (module-set! image-module 'polkit-provision (lambda _ (exit 77)))))
     (provision-image input output (load-manifest manifest)))))
  (_ (error "invalid test arguments")))
