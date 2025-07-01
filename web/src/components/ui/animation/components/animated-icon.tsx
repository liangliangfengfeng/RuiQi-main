// src/components/ui/animation/components/animated-icon.tsx
import { motion } from "motion/react"
import React, { forwardRef } from "react"
import {
    spinIconAnimation,
    continuousSpinAnimation,
    continuousShakeAnimation,
    continuousPulseAnimation,
    shakeIconAnimation,
    bounceIconAnimation,
    pulseIconAnimation,
    glowIconAnimation
} from "../icon-animation"

interface AnimatedIconProps {
    children: React.ReactNode
    className?: string
    animationVariant?: "spin" | "continuous-spin" | "continuous-shake" | "continuous-pulse" | "shake" | "bounce" | "pulse" | "glow"
    isAnimating?: boolean // 用于控制连续动画
    onClick?: () => void
}

export const AnimatedIcon = forwardRef<HTMLDivElement, AnimatedIconProps>(
    ({
        children,
        className = "",
        animationVariant = "spin",
        isAnimating = false,
        onClick,
        ...props
    }, ref) => {
        // 根据动画变体选择动画属性
        const getAnimationProps = () => {
            // 对于连续动画，根据 isAnimating 决定是否应用动画
            if ((animationVariant === "continuous-spin" || animationVariant === "continuous-shake" || animationVariant === "continuous-pulse") && isAnimating) {
                switch (animationVariant) {
                    case "continuous-spin":
                        return continuousSpinAnimation
                    case "continuous-shake":
                        return continuousShakeAnimation
                    case "continuous-pulse":
                        return continuousPulseAnimation
                    default:
                        return continuousSpinAnimation
                }
            }

            // 对于其他交互动画，根据变体选择
            switch (animationVariant) {
                case "continuous-spin":
                case "spin":
                    return spinIconAnimation
                case "continuous-shake":
                case "shake":
                    return shakeIconAnimation
                case "continuous-pulse":
                case "bounce":
                    return bounceIconAnimation
                case "pulse":
                    return pulseIconAnimation
                case "glow":
                    return glowIconAnimation
                default:
                    return spinIconAnimation
            }
        }

        return (
            <motion.div
                ref={ref}
                className={className}
                {...getAnimationProps()}
                onClick={onClick}
                {...props}
            >
                {children}
            </motion.div>
        )
    }
)

AnimatedIcon.displayName = "AnimatedIcon"