;;; SPDX-License-Identifier: MPL-2.0
(define-module (guix-secrets layout)
  #:use-module (gnu image)
  #:use-module (gnu system)
  #:use-module (gnu system image)
  #:use-module (gnu system file-systems)
  #:use-module (gnu system uuid)
  #:use-module (guix-secrets files)
  #:use-module (guix-secrets requests)
  #:use-module (ice-9 match)
  #:use-module (srfi srfi-1)
  #:export (root-partition-index destination-file-system provisioning-plan))

(define (root-partition-index image)
  (let ((roots (filter (lambda (p) (memq 'boot (partition-flags p)))
                       (image-partitions image))))
    (unless (= 1 (length roots))
      (throw 'secrets-error "image must declare exactly one root partition with the boot flag"))
    (list-index (lambda (p) (eq? p (car roots))) (image-partitions image))))

(define (under-mount? path mount)
  (or (string=? mount "/") (string=? path mount)
      (string-prefix? (string-append mount "/") path)))

(define (destination-file-system destination file-systems)
  (let ((matches (filter (lambda (fs)
                           (under-mount? destination (file-system-mount-point fs)))
                         file-systems)))
    (unless (pair? matches) (error "destination has no declared filesystem"))
    (car (sort matches (lambda (a b)
                         (> (string-length (file-system-mount-point a))
                            (string-length (file-system-mount-point b))))))))

(define (subvolume-options fs)
  (let ((options (file-system-options fs)))
    (if (and (string=? (file-system-type fs) "btrfs") options)
        (filter (lambda (s) (or (string-prefix? "subvol=" s)
                                (string-prefix? "subvolid=" s)))
                (string-split options #\,))
        '())))

(define (device-matches? device partition)
  (cond ((file-system-label? device)
         (equal? (file-system-label->string device) (partition-label partition)))
        ((uuid? device)
         (and (uuid? (partition-uuid partition))
              (string=? (uuid->string device) (uuid->string (partition-uuid partition)))))
        (else #f)))

(define (provisioning-plan image extents files)
  "Return data-only (OFFSET SIZE TYPE OPTIONS FILES) mount requests.
EXTENTS are actual artifact geometry in declaration order. Resolve all entries
before mounting anything; a separate/unsupported filesystem must not fall back
to a directory on root. Each filesystem is mounted independently."
  (validate-payload files)
  (unless (eq? (image-format image) 'disk-image)
    (throw 'secrets-error "expected a raw disk-image declaration"))
  (let* ((partitions (image-partitions image))
         (root-index (root-partition-index image))
         ;; Guix replaces root's device/type/options for image creation. In
         ;; particular, host-specific root subvol options must not be replayed
         ;; against an image whose boot configuration uses its default subvolume.
         (file-systems (operating-system-file-systems (operating-system-for-image image)))
         (groups '()))
    (unless (= (length extents) (length partitions))
      (throw 'secrets-error "artifact partition count does not match image declaration"))
    (for-each
     (match-lambda
       ((destination mode bytes)
        (let* ((fs (destination-file-system destination file-systems))
               (mount (file-system-mount-point fs))
               (type (file-system-type fs))
               (index (if (string=? mount "/") root-index
                          (let ((matches
                                 (filter-map (lambda (p i)
                                               (and (device-matches? (file-system-device fs) p) i))
                                             partitions (iota (length partitions)))))
                            (unless (= 1 (length matches))
                              (throw 'secrets-error "destination filesystem must identify one image partition by label or UUID"))
                            (car matches))))
               (partition (list-ref partitions index))
               (relative (if (string=? mount "/") destination
                             (substring destination (string-length mount))))
               (key (append (list-ref extents index) (list type (subvolume-options fs))))
               (group (assoc key groups)))
          (unless (and (supported-file-system? type)
                       (string=? type (partition-file-system partition)))
            (throw 'secrets-error "destination requires an unsupported or mismatched filesystem; supported: ext2, ext3, ext4, btrfs"))
          (unless (valid-destination? relative)
            (error "secret destination must be a file beneath its mount point"))
          (if group
              (set-cdr! group (append (cdr group) (list (list relative mode bytes))))
              (set! groups (append groups (list (cons key (list (list relative mode bytes))))))))))
     files)
    (map (lambda (group) (append (car group) (list (cdr group)))) groups)))
